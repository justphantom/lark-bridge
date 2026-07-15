package feishu

import (
	"testing"

	sdktypes "github.com/larksuite/oapi-sdk-go/v3/channel/types"
)

func TestStripMentionPlaceholders(t *testing.T) {
	bot := sdktypes.Mention{Key: "@_user_1", Name: "BridgeBot", OpenID: "bot-open-id", IsBot: true}
	alice := sdktypes.Mention{Key: "@_user_2", Name: "Alice", OpenID: "alice-open-id"}
	bob := sdktypes.Mention{Key: "@_user_3", Name: "Bob", OpenID: "bob-open-id"}

	t.Run("empty text or no mentions returns unchanged", func(t *testing.T) {
		if got := StripMentionPlaceholders("", nil); got != "" {
			t.Errorf("empty text = %q, want empty", got)
		}
		if got := StripMentionPlaceholders("hello", nil); got != "hello" {
			t.Errorf("no mentions = %q, want hello", got)
		}
	})

	t.Run("bot mention removed by IsBot flag", func(t *testing.T) {
		text := "@_user_1 please help"
		got := StripMentionPlaceholders(text, []sdktypes.Mention{bot})
		if got != "please help" {
			t.Errorf("got %q, want %q", got, "please help")
		}
	})

	t.Run("bot mention removed by IsBot flag (single-line)", func(t *testing.T) {
		text := "@_user_1 hi"
		got := StripMentionPlaceholders(text, []sdktypes.Mention{bot})
		if got != "hi" {
			t.Errorf("got %q, want %q", got, "hi")
		}
	})

	t.Run("user mention replaced with name", func(t *testing.T) {
		text := "@_user_2 says hi"
		got := StripMentionPlaceholders(text, []sdktypes.Mention{alice})
		if got != "@Alice says hi" {
			t.Errorf("got %q, want %q", got, "@Alice says hi")
		}
	})

	t.Run("user with empty name falls back to 用户", func(t *testing.T) {
		anon := sdktypes.Mention{Key: "@_user_2", OpenID: "x"}
		text := "@_user_2 hi"
		got := StripMentionPlaceholders(text, []sdktypes.Mention{anon})
		if got != "@用户 hi" {
			t.Errorf("got %q, want %q", got, "@用户 hi")
		}
	})

	t.Run("longest-placeholder-first avoids prefix corruption", func(t *testing.T) {
		// "@_user_1" is a prefix of "@_user_10"; naive in-order replace
		// would corrupt "@_user_10" into "@<Name1>0".
		u1 := sdktypes.Mention{Key: "@_user_1", Name: "First", OpenID: "id1"}
		u10 := sdktypes.Mention{Key: "@_user_10", Name: "Tenth", OpenID: "id10"}
		text := "@_user_10 and @_user_1"
		got := StripMentionPlaceholders(text, []sdktypes.Mention{u1, u10})
		if got != "@Tenth and @First" {
			t.Errorf("got %q, want %q (prefix collision)", got, "@Tenth and @First")
		}
	})

	t.Run("all-hands mention removed", func(t *testing.T) {
		all := sdktypes.Mention{Key: "@_all", OpenID: "all"}
		text := "@_all team update"
		got := StripMentionPlaceholders(text, []sdktypes.Mention{all})
		if got != "team update" {
			t.Errorf("got %q, want %q", got, "team update")
		}
	})

	t.Run("mixed bot + users", func(t *testing.T) {
		text := "@_user_1 @_user_2 @_user_3 check this"
		got := StripMentionPlaceholders(text, []sdktypes.Mention{bot, alice, bob})
		if got != "@Alice @Bob check this" {
			t.Errorf("got %q, want %q", got, "@Alice @Bob check this")
		}
	})

	t.Run("multi-line input preserves newlines", func(t *testing.T) {
		text := "@_user_1 line one\nline two\n\nline four"
		got := StripMentionPlaceholders(text, []sdktypes.Mention{bot})
		if want := "line one\nline two\n\nline four"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("horizontal whitespace collapsed, newlines kept", func(t *testing.T) {
		text := "@_user_1 hello   world\n   indented"
		got := StripMentionPlaceholders(text, []sdktypes.Mention{bot})
		if want := "hello world\nindented"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}
