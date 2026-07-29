package bridgebase

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/cmdutil"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
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

// waitCondTrue polls pred up to timeout, returning whether it ever became true.
// Used by the picker-path test below to wait for the AnswerBroker registration.
func waitCondTrue(timeout time.Duration, pred func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return pred()
}

// TestMakeEnumPicker_PickerRejectsInvalid verifies the picker path
// (len(args)==0) re-checks the delivered choice against cfg.Valid and refuses
// to persist a value Valid rejects. Without this guard, an operator misconfig
// (Options containing a value Valid rejects, e.g. an illegal --approval-mode)
// could be selected from the card and written to the binding, breaking the
// next CLI run. The pin must stay empty and the handler must return Handled.
func TestMakeEnumPicker_PickerRejectsInvalid(t *testing.T) {
	r, _ := router.New("", log.Nop())
	core := NewCore(r, nil, CoreConfig{}, log.Nop())
	defer core.AppCancel()
	h := &fakeEnumHolder{core: core, router: r}
	r.Bind("c1", "", "", "", "", "")

	acc := EnumPickerAccessors[*fakeEnumHolder]{Ensure: h.ensure, Get: h.get, Set: h.set}
	spec := MakeEnumPicker(core, EnumPickerConfig{
		Spec:       cmdutil.Spec{Name: "/effort"},
		FieldLabel: "level",
		// "BOGUS" is intentionally listed but rejected by Valid, modelling a
		// misconfigured option list.
		Options:   []string{"low", "medium", "BOGUS"},
		Default:   "medium",
		ErrorHint: "可选 low | medium | high",
		Valid:     func(v string) bool { return v == "low" || v == "medium" || v == "high" },
	}, acc)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, replyToIDKey{}, "reply-1")

	type res struct {
		r cmdutil.Result
		e error
	}
	resCh := make(chan res, 1)
	go func() {
		rr, ee := spec.Handler(h, ctx, "c1", nil) // picker path (no args)
		resCh <- res{rr, ee}
	}()

	if !waitCondTrue(time.Second, func() bool { return len(core.Answers.PendingIDs()) > 0 }) {
		t.Fatal("picker did not register a waiter")
	}
	reqID := core.Answers.PendingIDs()[0]
	if !core.Answers.Deliver(reqID, &protocol.AnswerPayload{Choices: []string{"BOGUS"}}) {
		t.Fatal("Deliver returned false; no waiter registered")
	}

	select {
	case got := <-resCh:
		if got.e != nil {
			t.Fatalf("handler returned error: %v", got.e)
		}
		if !got.r.Handled {
			t.Errorf("expected Handled=true (card patched in place), got %+v", got.r)
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not return after deliver")
	}

	if b, _ := r.Lookup("c1"); b.EffortLevel != "" {
		t.Errorf("EffortLevel = %q, want empty (invalid choice must not persist)", b.EffortLevel)
	}
}

// TestMakeEnumPicker_PickerAcceptsValid verifies the happy picker path still
// persists a legal choice — a regression guard so the new Valid gate does not
// accidentally reject values Options and Valid both accept.
func TestMakeEnumPicker_PickerAcceptsValid(t *testing.T) {
	r, _ := router.New("", log.Nop())
	core := NewCore(r, nil, CoreConfig{}, log.Nop())
	defer core.AppCancel()
	h := &fakeEnumHolder{core: core, router: r}
	r.Bind("c1", "", "", "", "", "")

	acc := EnumPickerAccessors[*fakeEnumHolder]{Ensure: h.ensure, Get: h.get, Set: h.set}
	spec := MakeEnumPicker(core, EnumPickerConfig{
		Spec:       cmdutil.Spec{Name: "/effort"},
		FieldLabel: "level",
		Options:    []string{"low", "medium", "high"},
		Default:    "medium",
		Valid:      func(v string) bool { return v == "low" || v == "medium" || v == "high" },
	}, acc)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, replyToIDKey{}, "reply-1")

	done := make(chan struct{})
	go func() {
		_, _ = spec.Handler(h, ctx, "c1", nil)
		close(done)
	}()

	if !waitCondTrue(time.Second, func() bool { return len(core.Answers.PendingIDs()) > 0 }) {
		t.Fatal("picker did not register a waiter")
	}
	reqID := core.Answers.PendingIDs()[0]
	core.Answers.Deliver(reqID, &protocol.AnswerPayload{Choices: []string{"high"}})

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not return after deliver")
	}
	if b, _ := r.Lookup("c1"); b.EffortLevel != "high" {
		t.Errorf("EffortLevel = %q, want high", b.EffortLevel)
	}
}
