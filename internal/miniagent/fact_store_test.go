package miniagent

import (
	"testing"
	"time"

	"github.com/hu/lark-bridge/internal/log"
)

func newTestFactStore(t *testing.T) *FileFactStore {
	t.Helper()
	return NewFactStore(t.TempDir(), log.Nop())
}

func TestFactStore_SetThenGet(t *testing.T) {
	s := newTestFactStore(t)
	if err := s.Set(ScopeChat, "c1", "lang", "zh", "test"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	f, ok, err := s.Get(ScopeChat, "c1", "lang")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatalf("Get missing")
	}
	if f.Value != "zh" || f.Source != "test" {
		t.Errorf("fact = %+v", f)
	}
	if f.UpdatedAt.IsZero() {
		t.Error("UpdatedAt not set")
	}
}

func TestFactStore_ScopeIsolation(t *testing.T) {
	s := newTestFactStore(t)
	_ = s.Set(ScopeChat, "c1", "k", "chat-val", "")
	_ = s.Set(ScopeGlobal, "c1", "k", "global-val", "")
	_ = s.Set(ScopeProject, "c1", "k", "project-val", "")

	for _, tc := range []struct {
		scope FactScope
		want  string
	}{
		{ScopeChat, "chat-val"},
		{ScopeGlobal, "global-val"},
		{ScopeProject, "project-val"},
	} {
		f, ok, err := s.Get(tc.scope, "c1", "k")
		if err != nil || !ok || f.Value != tc.want {
			t.Errorf("scope %s got (%v, %v, %v), want %s", tc.scope, f, ok, err, tc.want)
		}
	}
}

func TestFactStore_ChatIsolation(t *testing.T) {
	s := newTestFactStore(t)
	_ = s.Set(ScopeChat, "a", "k", "a-val", "")
	_ = s.Set(ScopeChat, "b", "k", "b-val", "")
	f, ok, _ := s.Get(ScopeChat, "a", "k")
	if !ok || f.Value != "a-val" {
		t.Errorf("chat a = %+v", f)
	}
	f, ok, _ = s.Get(ScopeChat, "b", "k")
	if !ok || f.Value != "b-val" {
		t.Errorf("chat b = %+v", f)
	}
}

func TestFactStore_ListAndPrefix(t *testing.T) {
	s := newTestFactStore(t)
	_ = s.Set(ScopeChat, "c", "user.lang", "zh", "")
	_ = s.Set(ScopeChat, "c", "user.role", "backend", "")
	_ = s.Set(ScopeChat, "c", "proj.name", "lark-bridge", "")

	all, err := s.List(ScopeChat, "c", "")
	if err != nil || len(all) != 3 {
		t.Fatalf("List all = %d, err %v", len(all), err)
	}
	pref, err := s.List(ScopeChat, "c", "user.")
	if err != nil || len(pref) != 2 {
		t.Fatalf("List prefix = %d, err %v", len(pref), err)
	}
}

func TestFactStore_Delete(t *testing.T) {
	s := newTestFactStore(t)
	_ = s.Set(ScopeChat, "c", "k", "v", "")
	if err := s.Delete(ScopeChat, "c", "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, ok, _ := s.Get(ScopeChat, "c", "k")
	if ok {
		t.Error("key still present after delete")
	}
}

func TestFactStore_Search(t *testing.T) {
	s := newTestFactStore(t)
	_ = s.Set(ScopeChat, "c", "lang", "prefer Chinese replies", "")
	_ = s.Set(ScopeChat, "c", "role", "senior backend engineer", "")
	_ = s.Set(ScopeChat, "c", "todo", "review auth middleware", "")

	res, err := s.Search(ScopeChat, "c", "engineer", 10)
	if err != nil || len(res) != 1 || res[0].Key != "role" {
		t.Fatalf("search engineer = %+v, err %v", res, err)
	}
	res, err = s.Search(ScopeChat, "c", "review", 10)
	if err != nil || len(res) != 1 || res[0].Key != "todo" {
		t.Fatalf("search review = %+v, err %v", res, err)
	}
}

func TestFactStore_NilSafe(t *testing.T) {
	var s *FileFactStore
	if got, _, err := s.Get(ScopeChat, "x", "k"); err != nil || got != (Fact{}) {
		t.Errorf("nil Get = %+v, %v", got, err)
	}
	if err := s.Set(ScopeChat, "x", "k", "v", ""); err != nil {
		t.Error("nil Set should be no-op")
	}
	if got, err := s.List(ScopeChat, "x", ""); err != nil || len(got) != 0 {
		t.Errorf("nil List = %+v, %v", got, err)
	}
}

func TestParseFactScope(t *testing.T) {
	if ParseFactScope("global") != ScopeGlobal {
		t.Error("global scope")
	}
	if ParseFactScope("") != ScopeChat {
		t.Error("empty scope defaults to chat")
	}
	if ParseFactScope("weird") != ScopeChat {
		t.Error("unknown scope defaults to chat")
	}
}

func TestFormatFacts(t *testing.T) {
	out := formatFacts([]Fact{{Key: "lang", Value: "zh"}})
	if out == "" || !stringsContains(out, "lang: zh") {
		t.Errorf("formatFacts = %q", out)
	}
	if formatFacts(nil) != "" {
		t.Error("formatFacts(nil) should be empty")
	}
}

func stringsContains(s, substr string) bool {
	return stringsContainsImpl(s, substr)
}

func stringsContainsImpl(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestFactStore_UpdateOverwrites(t *testing.T) {
	s := newTestFactStore(t)
	before := time.Now()
	_ = s.Set(ScopeChat, "c", "k", "v1", "")
	time.Sleep(5 * time.Millisecond)
	_ = s.Set(ScopeChat, "c", "k", "v2", "")
	f, _, _ := s.Get(ScopeChat, "c", "k")
	if f.Value != "v2" {
		t.Errorf("value = %q, want v2", f.Value)
	}
	if !f.UpdatedAt.After(before) {
		t.Error("UpdatedAt not refreshed")
	}
}
