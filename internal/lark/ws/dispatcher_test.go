package ws

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseMessageReceive_BotMentionByMentionedType is the regression test for
// the §2.1 fix: a mention with `mentioned_type:"app"` MUST be parsed with
// IsBot=true so StripMentionPlaceholders removes the @bot placeholder. The
// earlier hand-written client had the json tag wrong (`id.app` instead of
// `mentioned_type`), so bot mentions were never recognised.
func TestParseMessageReceive_BotMentionByMentionedType(t *testing.T) {
	payload := jsonRaw(t, map[string]any{
		"sender": map[string]any{"sender_id": map[string]any{"open_id": "ou_sender"}},
		"message": map[string]any{
			"message_id":   "om_1",
			"chat_id":      "oc_1",
			"message_type": "text",
			"content":      `{"text":"hi"}`,
			"mentions": []map[string]any{
				{"key": "@_user_1", "name": "Bot", "mentioned_type": "app", "id": map[string]any{"open_id": "ou_bot"}},
				{"key": "@_user_2", "name": "Alice", "mentioned_type": "user", "id": map[string]any{"open_id": "ou_alice"}},
			},
		},
	})
	ev, err := parseMessageReceive("evt_1", payload)
	if err != nil {
		t.Fatalf("parseMessageReceive: %v", err)
	}
	if len(ev.Mentions) != 2 {
		t.Fatalf("mentions len = %d, want 2", len(ev.Mentions))
	}
	if !ev.Mentions[0].IsBot {
		t.Errorf("mention[0] IsBot = false; `mentioned_type:\"app\"` must mark the bot mention (regression of the id.app tag bug)")
	}
	if ev.Mentions[0].OpenID != "ou_bot" {
		t.Errorf("mention[0] OpenID = %q want ou_bot", ev.Mentions[0].OpenID)
	}
	if ev.Mentions[1].IsBot {
		t.Errorf("mention[1] IsBot = true; a user mention must not be flagged")
	}
}

// TestParseMessageReceive_IsBotField covers the defensive is_bot=true path
// (some payloads may carry it directly).
func TestParseMessageReceive_IsBotField(t *testing.T) {
	payload := jsonRaw(t, map[string]any{
		"message": map[string]any{
			"message_type": "text",
			"content":      `{"text":"x"}`,
			"mentions": []map[string]any{
				{"key": "@_user_1", "is_bot": true, "id": map[string]any{"open_id": "ou_b"}},
			},
		},
	})
	ev, err := parseMessageReceive("evt", payload)
	if err != nil {
		t.Fatalf("parseMessageReceive: %v", err)
	}
	if !ev.Mentions[0].IsBot {
		t.Errorf("is_bot:true must yield IsBot=true")
	}
}

// TestParseMessageReceive_CreateTimeMsString verifies the v2 string-typed
// create_time is parsed into Unix milliseconds.
func TestParseMessageReceive_CreateTimeMsString(t *testing.T) {
	payload := jsonRaw(t, map[string]any{
		"message": map[string]any{
			"message_type": "text",
			"content":      `{"text":"x"}`,
			"create_time":  "1700000000123",
		},
	})
	ev, err := parseMessageReceive("evt", payload)
	if err != nil {
		t.Fatalf("parseMessageReceive: %v", err)
	}
	if ev.CreateTimeMs != 1700000000123 {
		t.Errorf("CreateTimeMs = %d, want 1700000000123", ev.CreateTimeMs)
	}
}

// TestParseCardAction_ContextIdentity verifies ChatID/MessageID come from
// context.open_chat_id / context.open_message_id (the §2.4 wire shape).
func TestParseCardAction_ContextIdentity(t *testing.T) {
	payload := jsonRaw(t, map[string]any{
		"operator": map[string]any{"open_id": "ou_op"},
		"action":   map[string]any{"value": map[string]any{"k": "v"}, "form_value": map[string]any{"q": "a"}},
		"context":  map[string]any{"open_message_id": "om_c", "open_chat_id": "oc_c"},
	})
	ev, err := parseCardAction("evt_c", payload)
	if err != nil {
		t.Fatalf("parseCardAction: %v", err)
	}
	if ev.ChatID != "oc_c" || ev.MessageID != "om_c" || ev.Operator.OpenID != "ou_op" {
		t.Fatalf("identity mismatch: %+v", ev)
	}
	if ev.Action.Value["k"] != "v" || ev.Action.FormValue["q"] != "a" {
		t.Fatalf("action payload mismatch: %+v", ev.Action)
	}
}

// TestReassembler_DuplicateSeqOverwrite pins the chunk-slot overwrite rule:
// a repeated seq for the same message_id replaces (does not double-count),
// matching the SDK's combine behaviour.
func TestReassembler_DuplicateSeqOverwrite(t *testing.T) {
	r := newReassembler()
	r.feed("om", 2, 0, []byte("a"))
	// Feed seq=0 again: must not increment got to 2 (sum is 2, but only seq=1
	// is still missing).
	if joined, ok := r.feed("om", 2, 0, []byte("a")); ok || joined != nil {
		t.Fatalf("duplicate seq=0 should not complete the group, got ok=%v joined=%q", ok, joined)
	}
	if joined, ok := r.feed("om", 2, 1, []byte("b")); !ok || string(joined) != "ab" {
		t.Fatalf("after seq=1, joined=%q ok=%v want \"ab\"", joined, ok)
	}
}

func jsonRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// keep strings import explicit (used by future helpers in this file).
var _ = strings.TrimSpace
