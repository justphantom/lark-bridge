package cmdutil

import (
	"reflect"
	"sort"
	"testing"
)

func TestFilterChildEnv_StripsSecrets(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"FEISHU_APP_ID=cli_x",          // not secret (id, not secret)
		"FEISHU_APP_SECRET=s3cr3t",     // SECRET → strip
		"IPC_SECRET=abc",               // SECRET → strip
		"ENCRYPT_KEY=k",                // ENCRYPT → strip
		"OPENCODE_SERVER_PASSWORD=p",   // PASS → strip
		"ANTHROPIC_API_KEY=sk-ant-xxx", // keeps (CLI needs it)
		"MINIAGENT_API_KEY=nvapi-xxx",  // keeps (CLI needs it)
		"SOME_TOKEN=t",                 // TOKEN → strip
		"MY_PRIVATE_KEY=pk",            // PRIVATE_KEY → strip
		"HOME=/root",
		"DATABASE_CREDENTIAL=cred", // CREDENTIAL → strip
	}
	got := FilterChildEnv(in)
	want := []string{
		"PATH=/usr/bin",
		"FEISHU_APP_ID=cli_x",
		"ANTHROPIC_API_KEY=sk-ant-xxx",
		"MINIAGENT_API_KEY=nvapi-xxx",
		"HOME=/root",
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FilterChildEnv mismatch:\n got = %v\n want = %v", got, want)
	}
}

func TestFilterChildEnv_EmptyAndMalformed(t *testing.T) {
	// Entries without '=' are treated as key-only and still filtered.
	got := FilterChildEnv([]string{"", "PLAINSECRET", "OK=1"})
	want := []string{"", "OK=1"} // PLAINSECRET matches SECRET → stripped
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

func TestSanitizeChildEnv_NeverPanicsAndStripsKnownSecret(t *testing.T) {
	t.Setenv("LARK_BRIDGE_TEST_SECRET", "shh")
	got := SanitizeChildEnv()
	for _, kv := range got {
		if kv == "LARK_BRIDGE_TEST_SECRET=shh" {
			t.Fatalf("SanitizeChildEnv did not strip the test secret")
		}
	}
}
