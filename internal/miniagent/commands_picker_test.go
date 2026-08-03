package miniagent

import "testing"

// TestSameModelID pins the display-only model matching used by /models to mark
// the "→ current" row. miniagent v3.3.0 (256c875) made -list-models emit
// "provider/model_id" under multi-provider configs and bare "model_id" under
// single-provider; the current pin (activeModel) can be either form. The match
// must not conflate two different providers that share a model id.
func TestSameModelID(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		// Both bare ids — plain equality.
		{"both bare equal", "gpt-4o", "gpt-4o", true},
		{"both bare differ", "gpt-4o", "gpt-4o-mini", false},

		// Cross-form (bare vs "provider/"-prefixed) NEVER matches. A bare
		// current pin (the global default cfgModel is always bare) meets a
		// prefixed list row only in a multi-provider config with a bare
		// default; matching on the id segment alone would light up every
		// provider sharing the id, so refuse to match instead.
		{"bare vs prefixed", "gpt-4o", "default/gpt-4o", false},
		{"prefixed vs bare", "default/gpt-4o", "gpt-4o", false},
		{"bare vs prefixed differ", "gpt-4o", "default/gpt-4o-mini", false},

		// Both prefixed (multi-provider path): the full spec must match, so
		// two providers sharing a model id are NOT conflated.
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
