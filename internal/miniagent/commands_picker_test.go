package miniagent

import "testing"

// TestSameModelID pins the display-only model matching used by /models to mark
// the "→ current" row. Both inputs are expected to be in the same form because
// cmdModels canonicalizes a bare current spec to "provider/model_id" when the
// list is known to come from a single provider.
func TestSameModelID(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		// Both bare ids — plain equality.
		{"both bare equal", "gpt-4o", "gpt-4o", true},
		{"both bare differ", "gpt-4o", "gpt-4o-mini", false},

		// Cross-form (bare vs "provider/"-prefixed) NEVER matches here.
		{"bare vs prefixed", "gpt-4o", "default/gpt-4o", false},
		{"prefixed vs bare", "default/gpt-4o", "gpt-4o", false},
		{"bare vs prefixed differ", "gpt-4o", "default/gpt-4o-mini", false},

		// Both prefixed: the full spec must match, so two providers sharing a
		// model id are NOT conflated.
		{"both prefixed same provider", "main/gpt-4o", "main/gpt-4o", true},
		{"both prefixed differ provider", "main/gpt-4o", "alt/gpt-4o", false},
		{"both prefixed differ model", "main/gpt-4o", "main/gpt-4o-mini", false},
	}
	for _, c := range cases {
		if got := sameModelID(c.a, c.b); got != c.want {
			t.Errorf("%s: sameModelID(%q, %q) = %v, want %v", c.name, c.a, c.b, got, c.want)
		}
	}
}

func TestCanonicalModelID(t *testing.T) {
	cases := []struct {
		name   string
		cur    string
		models []string
		want   string
	}{
		{
			name:   "already prefixed unchanged",
			cur:    "default/gpt-4o",
			models: []string{"default/gpt-4o", "default/gpt-4o-mini"},
			want:   "default/gpt-4o",
		},
		{
			name:   "single provider canonicalizes bare cur",
			cur:    "gpt-4o",
			models: []string{"default/gpt-4o", "default/gpt-4o-mini"},
			want:   "default/gpt-4o",
		},
		{
			name:   "single provider but cur not in list stays bare",
			cur:    "gpt-4",
			models: []string{"default/gpt-4o", "default/gpt-4o-mini"},
			want:   "gpt-4",
		},
		{
			name:   "multi-provider bare cur stays bare",
			cur:    "gpt-4o",
			models: []string{"main/gpt-4o", "alt/gpt-4o"},
			want:   "gpt-4o",
		},
		{
			name:   "mixed bare and prefixed stays bare",
			cur:    "gpt-4o",
			models: []string{"gpt-4o", "default/gpt-4o-mini"},
			want:   "gpt-4o",
		},
		{
			name:   "empty list leaves cur unchanged",
			cur:    "gpt-4o",
			models: []string{},
			want:   "gpt-4o",
		},
	}
	for _, c := range cases {
		if got := canonicalModelID(c.cur, c.models); got != c.want {
			t.Errorf("%s: canonicalModelID(%q, %v) = %q, want %q", c.name, c.cur, c.models, got, c.want)
		}
	}
}

func TestCommonProviderPrefix(t *testing.T) {
	cases := []struct {
		name   string
		models []string
		wantP  string
		wantOK bool
	}{
		{"single provider", []string{"p/a", "p/b"}, "p", true},
		{"single provider with empty", []string{"p/a", "", "p/b"}, "p", true},
		{"multi provider", []string{"p/a", "q/b"}, "", false},
		{"bare entry", []string{"p/a", "b"}, "", false},
		{"empty list", []string{}, "", false},
		{"all empty", []string{"", ""}, "", false},
	}
	for _, c := range cases {
		p, ok := commonProviderPrefix(c.models)
		if p != c.wantP || ok != c.wantOK {
			t.Errorf("%s: commonProviderPrefix(%v) = (%q, %v), want (%q, %v)", c.name, c.models, p, ok, c.wantP, c.wantOK)
		}
	}
}
