package bridgebase

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/cmdutil"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/router"
)

// fakeEnumHolder is a stand-in for a bridge Handler with one string pin.
type fakeEnumHolder struct {
	core   *Core
	router *router.Router
}

func (f *fakeEnumHolder) ensure(_ *fakeEnumHolder, chatID string) error {
	f.router.Bind(chatID, "", "", "", "", "")
	return nil
}
func (f *fakeEnumHolder) get(_ *fakeEnumHolder, chatID string) string {
	b, _ := f.router.Lookup(chatID)
	return b.EffortLevel
}
func (f *fakeEnumHolder) set(_ *fakeEnumHolder, chatID, v string) {
	f.router.SetEffortLevel(chatID, v)
	cmdutil.LogSettingChange(f.core.Logger, chatID, "test_enum", v)
}

// TestMakeEnumPicker_DirectPin verifies the direct-pin path: a valid value
// is set and a ChangeResult returned.
func TestMakeEnumPicker_DirectPin(t *testing.T) {
	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatal(err)
	}
	core := NewCore(r, nil, CoreConfig{}, log.Nop())
	h := &fakeEnumHolder{core: core, router: r}
	r.Bind("c1", "", "", "", "", "")

	acc := EnumPickerAccessors[*fakeEnumHolder]{
		Ensure: h.ensure,
		Get:    h.get,
		Set:    h.set,
	}
	spec := MakeEnumPicker(core, EnumPickerConfig{
		Spec:       cmdutil.Spec{Name: "/effort", Level: "success"},
		FieldLabel: "level",
		LogKey:     "test_enum",
		Options:    []string{"low", "medium", "high"},
		Default:    "medium",
		ErrorHint:  "可选 low | medium | high",
		Valid:      func(v string) bool { return v == "low" || v == "medium" || v == "high" },
	}, acc)

	res, err := spec.Handler(h, context.Background(), "c1", []string{"high"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.Field != "level" || res.After != "high" {
		t.Errorf("result = %+v, want Field=level After=high", res)
	}
	if b, _ := r.Lookup("c1"); b.EffortLevel != "high" {
		t.Errorf("EffortLevel = %q, want high", b.EffortLevel)
	}
}

// TestMakeEnumPicker_InvalidValue verifies the unknown-value path returns an
// error result without mutating the pin.
func TestMakeEnumPicker_InvalidValue(t *testing.T) {
	r, _ := router.New("", log.Nop())
	core := NewCore(r, nil, CoreConfig{}, log.Nop())
	h := &fakeEnumHolder{core: core, router: r}
	r.Bind("c1", "", "", "", "", "")

	acc := EnumPickerAccessors[*fakeEnumHolder]{Get: h.get, Set: h.set}
	spec := MakeEnumPicker(core, EnumPickerConfig{
		Spec:       cmdutil.Spec{Name: "/effort"},
		FieldLabel: "level",
		Options:    []string{"low"},
		Valid:      func(v string) bool { return v == "low" },
	}, acc)

	res, err := spec.Handler(h, context.Background(), "c1", []string{"bogus"})
	// Both result and error carry the unknown-value message; the dispatcher's
	// generic error path will surface it as a level=error notice.
	if err == nil {
		t.Fatal("expected error for bogus value")
	}
	if !strings.Contains(res.Body, "未知") && !strings.Contains(err.Error(), "未知") {
		t.Errorf("missing 未知 in body=%q err=%v", res.Body, err)
	}
	if b, _ := r.Lookup("c1"); b.EffortLevel != "" {
		t.Errorf("EffortLevel mutated on invalid: %q", b.EffortLevel)
	}
}

// TestMakeEnumPicker_Clear verifies the clear path resets the pin and reports
// the default fallback.
func TestMakeEnumPicker_Clear(t *testing.T) {
	r, _ := router.New("", log.Nop())
	core := NewCore(r, nil, CoreConfig{}, log.Nop())
	h := &fakeEnumHolder{core: core, router: r}
	r.Bind("c1", "", "", "", "", "")
	h.router.SetEffortLevel("c1", "high")

	acc := EnumPickerAccessors[*fakeEnumHolder]{Get: h.get, Set: h.set}
	spec := MakeEnumPicker(core, EnumPickerConfig{
		Spec:       cmdutil.Spec{Name: "/effort"},
		FieldLabel: "level",
		Default:    "medium",
	}, acc)

	res, err := spec.Handler(h, context.Background(), "c1", []string{"clear"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.After != "默认 (medium)" {
		t.Errorf("After = %q, want '默认 (medium)'", res.After)
	}
	if b, _ := r.Lookup("c1"); b.EffortLevel != "" {
		t.Errorf("EffortLevel = %q, want cleared", b.EffortLevel)
	}
	// Silence unused warning on time/filepath if we strip the imports later.
	_ = time.Second
	_ = filepath.Separator
}
