// Package config loads and validates per-service bridge configuration from
// a JSON file.
//
// The wire format is one union document (deploy.sh copies a single
// base-config.json to every service), but the Go surface is per-service:
// each binary decodes ONLY the top-level keys it owns via its LoadXxx
// function. Foreign sections (owned by another service) are skipped, so a
// config edited for a newer miniagent-back still loads on an older
// feishu-front — the D5 "all binaries same version" constraint relaxes to
// "each service's own sections must match its binary". Two protections are
// preserved from the old union decode:
//
//   - a typo'd TOP-LEVEL key is rejected by every service (the known-key set
//     is the union of all service structs, derived by reflection), and
//   - a typo'd key INSIDE an owned section is rejected by DisallowUnknownFields
//     on the filtered document.
//
// Pipeline: readRaw -> secretPermWarnings -> expandEnvVars -> split top-level
// keys -> filter to owned (reject unknown) -> strict decode -> applyDefaults
// (core then service) -> validate (core then service).
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/strutil"
)

// envVarPattern is the shared ${VAR} matcher defined once in strutil so the
// config loader and strutil.ExpandEnvVars cannot drift apart on the surface
// syntax.
var envVarPattern = strutil.EnvVarPattern

// Duration is a time.Duration that JSON-encodes as a Go duration
// string ("5m", "60s") rather than nanoseconds. It is a named type
// because Go does not allow methods on time.Duration itself.
type Duration time.Duration

// UnmarshalJSON parses a Go duration string. A field absent from the
// JSON stays at its zero value (0) and is filled by applyDefaults; an
// explicitly-supplied non-positive value ("0", "-5s") is rejected
// here so it cannot be silently overwritten by applyDefaults.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration: expect a string like %q: %w", "5m", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration: parse %q: %w", s, err)
	}
	if parsed <= 0 {
		return fmt.Errorf("duration: %q must be positive", s)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalJSON emits the duration as a Go duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// LoadFeishuFront reads and validates the feishu-front configuration.
func LoadFeishuFront(path string) (*FeishuFrontConfig, error) {
	cfg, _, err := LoadFeishuFrontWithWarnings(path)
	return cfg, err
}

// LoadFeishuFrontWithWarnings is LoadFeishuFront plus non-fatal operator
// warnings (see loadService). Callers should log each warning at startup.
func LoadFeishuFrontWithWarnings(path string) (*FeishuFrontConfig, []string, error) {
	var cfg FeishuFrontConfig
	warnings, err := loadService(path, &cfg)
	if err != nil {
		return nil, nil, err
	}
	applyCoreDefaults(&cfg.Core, path)
	applyFeishuDefaults(&cfg)
	if err := validateCore(&cfg.Core); err != nil {
		return nil, nil, fmt.Errorf("validate: %w", err)
	}
	if err := validateFeishu(&cfg); err != nil {
		return nil, nil, fmt.Errorf("validate: %w", err)
	}
	return &cfg, warnings, nil
}

// LoadMiniAgentBack reads and validates the miniagent-back configuration.
func LoadMiniAgentBack(path string) (*MiniAgentBackConfig, error) {
	cfg, _, err := LoadMiniAgentBackWithWarnings(path)
	return cfg, err
}

// LoadMiniAgentBackWithWarnings is LoadMiniAgentBack plus non-fatal operator
// warnings (see loadService).
func LoadMiniAgentBackWithWarnings(path string) (*MiniAgentBackConfig, []string, error) {
	var cfg MiniAgentBackConfig
	warnings, err := loadService(path, &cfg)
	if err != nil {
		return nil, nil, err
	}
	applyCoreDefaults(&cfg.Core, path)
	applyMiniAgentDefaults(&cfg)
	applyStatusMonitorDefaults(&cfg.StatusMonitor)
	if err := validateCore(&cfg.Core); err != nil {
		return nil, nil, fmt.Errorf("validate: %w", err)
	}
	if err := validateMiniAgent(&cfg); err != nil {
		return nil, nil, fmt.Errorf("validate: %w", err)
	}
	return &cfg, warnings, nil
}

// LoadStatusMonitor reads and validates the status-monitor configuration.
func LoadStatusMonitor(path string) (*StatusMonitorConfig, error) {
	cfg, _, err := LoadStatusMonitorWithWarnings(path)
	return cfg, err
}

// LoadStatusMonitorWithWarnings is LoadStatusMonitor plus non-fatal operator
// warnings (see loadService).
func LoadStatusMonitorWithWarnings(path string) (*StatusMonitorConfig, []string, error) {
	var cfg StatusMonitorConfig
	warnings, err := loadService(path, &cfg)
	if err != nil {
		return nil, nil, err
	}
	applyCoreDefaults(&cfg.Core, path)
	applyMonitorDefaults(&cfg)
	if err := validateCore(&cfg.Core); err != nil {
		return nil, nil, fmt.Errorf("validate: %w", err)
	}
	return &cfg, warnings, nil
}

// loadService performs the shared decode pipeline for one service config:
// read, warn, expand ${VAR}, split the document into top-level keys, drop
// keys owned by OTHER services, then strict-decode (DisallowUnknownFields)
// only the owned subset into out. A top-level key that no service struct
// knows is a typo (or a key from a foreign lark-bridge version) and is
// rejected outright so operators do not believe an edited config took
// effect. Defaults and validation are the caller's (per-service) job.
func loadService(path string, out any) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	warnings := secretPermWarnings(path, raw)
	expanded, err := expandEnvVars(raw)
	if err != nil {
		return nil, fmt.Errorf("expand env: %w", err)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(expanded, &top); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}

	owned := ownedKeys(reflect.TypeOf(out).Elem())
	known := allKnownKeys()
	for key := range top {
		if _, ok := known[key]; !ok {
			return nil, fmt.Errorf("parse json: unknown top-level key %q (typo, or written for a newer lark-bridge)", key)
		}
		if _, ok := owned[key]; !ok {
			delete(top, key) // another service's section: not ours to decode
		}
	}
	filtered, err := json.Marshal(top)
	if err != nil {
		return nil, fmt.Errorf("filter sections: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(filtered))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}
	return warnings, nil
}

// ownedKeys returns the top-level JSON keys decoded into a struct of type t:
// named fields contribute their json tag; embedded anonymous structs recurse
// (flat promotion, e.g. Core inside MiniAgentBackConfig). Used by loadService
// to decide which sections of the union document belong to the loading
// service.
func ownedKeys(t reflect.Type) map[string]struct{} {
	keys := map[string]struct{}{}
	var walk func(rt reflect.Type)
	walk = func(rt reflect.Type) {
		for i := range rt.NumField() {
			f := rt.Field(i)
			if f.Anonymous && f.Type.Kind() == reflect.Struct && f.Tag.Get("json") == "" {
				walk(f.Type) // untagged embed: its fields decode flat at the top level
				continue
			}
			if f.PkgPath != "" {
				continue // unexported
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			keys[name] = struct{}{}
		}
	}
	walk(t)
	return keys
}

// allKnownKeys is the union of every service struct's top-level keys — the
// typo-detection vocabulary. Derived by reflection (not hand-maintained) so
// a field added to any service config is automatically tolerated by the
// other services' binaries built from the same tree.
func allKnownKeys() map[string]struct{} {
	keys := map[string]struct{}{}
	for _, t := range []reflect.Type{
		reflect.TypeFor[FeishuFrontConfig](),
		reflect.TypeFor[MiniAgentBackConfig](),
		reflect.TypeFor[StatusMonitorConfig](),
	} {
		for k := range ownedKeys(t) {
			keys[k] = struct{}{}
		}
	}
	return keys
}

// expandEnvVars replaces ${VAR} patterns in raw config bytes with env
// values. Returns an error if any referenced variable is unset or empty.
//
// The replacement value is JSON-string-escaped before splicing so a
// secret containing `"`, `\`, or control characters cannot break the
// surrounding JSON (it is always interpolated inside a JSON string
// value, since ${VAR} only appears in a string-typed config field).
func expandEnvVars(data []byte) ([]byte, error) {
	matches := envVarPattern.FindAllSubmatchIndex(data, -1)
	if matches == nil {
		return data, nil
	}

	var out []byte
	last := 0
	for _, m := range matches {
		out = append(out, data[last:m[0]]...)
		name := string(data[m[2]:m[3]])
		val, ok := os.LookupEnv(name)
		if !ok {
			return nil, fmt.Errorf("config: env var ${%s} is unset (set it in bridge.env)", name)
		}
		if val == "" {
			return nil, fmt.Errorf("config: env var ${%s} is set but empty (check bridge.env)", name)
		}
		// JSON-escape the value so quotes/backslashes/control chars in a
		// secret do not corrupt the surrounding JSON document.
		escaped, err := json.Marshal(val)
		if err != nil {
			return nil, fmt.Errorf("config: escape env var ${%s}: %w", name, err)
		}
		// Marshal wraps the string in quotes; strip them because we are
		// splicing into an already-quoted JSON string literal.
		out = append(out, escaped[1:len(escaped)-1]...)
		last = m[1]
	}
	return append(out, data[last:]...), nil
}

// plaintextSecretRe matches a secret-bearing JSON key whose VALUE is a
// literal (not a "${VAR}" reference): feishu_app_secret / ipc_secret /
// miniagent api_key written in cleartext.
var plaintextSecretRe = regexp.MustCompile(`"(?:feishu_app_secret|ipc_secret|api_key)"\s*:\s*"((?:[^"$]|\$[^{])[a-zA-Z0-9_-]*)"`)

// secretPermWarnings returns a warning when the config file is
// group/other-readable AND embeds at least one plaintext secret (low-20).
// Permission alone is not warned on (the file may hold only ${VAR}
// references); plaintext alone is fine when the file is 0600. Skipped on
// non-Unix platforms where Perm bits are not meaningful.
func secretPermWarnings(path string, raw []byte) []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil // the read already succeeded; a stat race is not warn-worthy
	}
	if info.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if !plaintextSecretRe.Match(raw) {
		return nil
	}
	return []string{fmt.Sprintf("config file %s is readable by group/other (mode %04o) and contains plaintext secrets; chmod 600 or switch to ${VAR} references", path, info.Mode().Perm())}
}
