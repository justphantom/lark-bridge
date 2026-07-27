package ws

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Sink is the consumer of reassembled WS events. The ws package owns these
// parsed wire-level types; the high-level lark package adapts them into its
// public API types (breaking what would otherwise be an import cycle between
// lark and lark/ws).
//
// OnCard returns an optional business response payload (the card/toast JSON
// Feishu expects in the card.action.trigger ACK). A nil slice means "no
// business payload" — the ACK carries only {code:200}. Non-nil slices are
// placed under the ACK's "data" field so Feishu updates the card client-side
// without needing a separate Patch call (and without Feishu's 3-second
// rollback-of-invalid-ACK fallback biting us).
type Sink interface {
	OnMessage(ctx context.Context, ev *MessageReceive) error
	OnCard(ctx context.Context, ev *CardAction) ([]byte, error)
}

// MessageReceive is the ws-level parsed form of im.message.receive_v1.
type MessageReceive struct {
	EventID      string
	MessageID    string
	ChatID       string
	ChatType     string
	MsgType      string
	Content      string
	CreateTimeMs int64
	SenderOpenID string
	Mentions     []Mention
}

// CardAction is the ws-level parsed form of card.action.trigger.
type CardAction struct {
	EventID   string
	ChatID    string
	MessageID string
	Operator  CardActionOperator
	Action    CardActionPayload
}

// CardActionOperator carries the user who clicked.
type CardActionOperator struct{ OpenID string }

// CardActionPayload carries the button/form data attached to the click.
type CardActionPayload struct {
	Value     map[string]any
	FormValue map[string]any
}

// Mention is one parsed user mention inside a received message.
type Mention struct {
	Key    string
	OpenID string
	Name   string
	IsBot  bool
}

// chunkTTL bounds how long a partial chunk group lingers in the reassembler
// before being garbage-collected. Matches the SDK's 5-second combine cache.
const chunkTTL = 5 * time.Second

// reassembler recombines lark's chunked data frames back into a single
// payload. Frames carrying the same message_id are split into sum pieces
// (seq 0..sum-1); unsplit frames have sum<=1 and bypass the buffer entirely.
// A periodic sweep drops incomplete groups older than chunkTTL so a lost tail
// chunk cannot leak memory indefinitely.
type reassembler struct {
	mu      sync.Mutex
	pending map[string]*pendingChunks
}

type pendingChunks struct {
	chunks    [][]byte
	got       int
	firstSeen time.Time
}

func newReassembler() *reassembler { return &reassembler{pending: make(map[string]*pendingChunks)} }

// maxReassembleChunks bounds the "sum" header value the reassembler will
// honour. The server splits only very large payloads (a handful of chunks at
// most); a malicious/buggy peer claiming sum=1e9 would otherwise allocate an
// 8 GB slice header on the first chunk. Anything above this is treated as a
// malformed group: dropped (the partial state is swept within chunkTTL).
const maxReassembleChunks = 256

// feed returns the joined payload when this frame completes the group, or
// nil (with ok=false) when more chunks are still outstanding. sum<=1 short-
// circuits to the payload itself (no buffering).
//
// A sum above maxReassembleChunks is rejected outright (no allocation): the
// frame is dropped silently rather than buffered, since a legit payload never
// exceeds the cap. seq out of range is ignored (does not count toward got).
func (r *reassembler) feed(msgID string, sum, seq int, payload []byte) (joined []byte, ok bool) {
	if sum <= 1 {
		return payload, true
	}
	if sum > maxReassembleChunks {
		// Refuse to buffer an unbounded group; treat as undeliverable.
		return nil, false
	}
	if msgID == "" {
		// No correlation id with sum>1: cannot recombine safely. Deliver as-is
		// (the server will not split a frame without a message_id in practice).
		return payload, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.pending[msgID]
	if p == nil {
		p = &pendingChunks{chunks: make([][]byte, sum), firstSeen: time.Now()}
		r.pending[msgID] = p
	}
	if seq >= 0 && seq < len(p.chunks) && p.chunks[seq] == nil {
		p.chunks[seq] = append([]byte(nil), payload...)
		p.got++
	}
	if p.got == sum {
		joined = bytes.Join(p.chunks, nil)
		delete(r.pending, msgID)
		return joined, true
	}
	return nil, false
}

// sweep drops pending groups older than maxAge. Called on a timer by the WS
// client; cheap because the map is normally empty (unsplit messages bypass it).
func (r *reassembler) sweep(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, p := range r.pending {
		if p.firstSeen.Before(cutoff) {
			delete(r.pending, id)
		}
	}
}

// router dispatches a reassembled event payload to the registered Sink.
// It parses the lark v2 envelope once and routes on header.event_type. Event
// types the bridge does not consume are dropped silently (matching the SDK's
// default branch) — the caller still ACKs the frame at the transport layer.
type router struct{ sink Sink }

// envelope is the common v2 header every WS event carries.
type envelope struct {
	Header struct {
		EventID   string `json:"event_id"`
		EventType string `json:"event_type"`
	} `json:"header"`
	Event json.RawMessage `json:"event"`
}

// dispatch parses payload and invokes the matching handler method. Errors are
// returned so the transport can build a 500 ACK; nil error → 200 ACK.
func (rt *router) dispatch(ctx context.Context, payload []byte) ([]byte, error) {
	if rt.sink == nil {
		return nil, nil
	}
	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("ws: parse envelope: %w", err)
	}
	switch env.Header.EventType {
	case "im.message.receive_v1":
		ev, err := parseMessageReceive(env.Header.EventID, env.Event)
		if err != nil {
			return nil, err
		}
		return nil, rt.sink.OnMessage(ctx, ev)
	case "card.action.trigger":
		ev, err := parseCardAction(env.Header.EventID, env.Event)
		if err != nil {
			return nil, err
		}
		return rt.sink.OnCard(ctx, ev)
	default:
		return nil, nil
	}
}

// receiveEvent mirrors the event.* block of im.message.receive_v1.
type receiveEvent struct {
	Sender struct {
		SenderID struct {
			OpenID string `json:"open_id"`
		} `json:"sender_id"`
	} `json:"sender"`
	Message struct {
		MessageID   string `json:"message_id"`
		CreateTime  string `json:"create_time"`
		ChatID      string `json:"chat_id"`
		ChatType    string `json:"chat_type"`
		MessageType string `json:"message_type"`
		Content     string `json:"content"`
		Mentions    []struct {
			Key  string `json:"key"`
			Name string `json:"name"`
			ID   struct {
				OpenID string `json:"open_id"`
			} `json:"id"`
			MentionedType string `json:"mentioned_type"` // "app" when the mention targets the bot itself
			IsBot         bool   `json:"is_bot"`         // newer field some payloads omit; MentionedType is the primary signal
		} `json:"mentions"`
	} `json:"message"`
}

// parseMessageReceive builds the ws-level MessageReceive from raw JSON.
func parseMessageReceive(eventID string, raw json.RawMessage) (*MessageReceive, error) {
	var re receiveEvent
	if err := json.Unmarshal(raw, &re); err != nil {
		return nil, fmt.Errorf("ws: parse message event: %w", err)
	}
	var createMs int64
	if re.Message.CreateTime != "" {
		// CreateTime is a string of unix-ms per the v2 schema.
		for _, c := range re.Message.CreateTime {
			if c < '0' || c > '9' {
				createMs = 0
				break
			}
			createMs = createMs*10 + int64(c-'0')
		}
	}
	mentions := make([]Mention, 0, len(re.Message.Mentions))
	for _, m := range re.Message.Mentions {
		mm := Mention{Key: m.Key, Name: m.Name, OpenID: m.ID.OpenID}
		// MentionedType arrives as the literal "app" when the mention targets
		// the bot itself; is_bot is a newer field some payloads omit.
		if m.IsBot || strings.EqualFold(m.MentionedType, "app") {
			mm.IsBot = true
		}
		mentions = append(mentions, mm)
	}
	return &MessageReceive{
		EventID:      eventID,
		MessageID:    re.Message.MessageID,
		ChatID:       re.Message.ChatID,
		ChatType:     re.Message.ChatType,
		MsgType:      re.Message.MessageType,
		Content:      re.Message.Content,
		CreateTimeMs: createMs,
		SenderOpenID: re.Sender.SenderID.OpenID,
		Mentions:     mentions,
	}, nil
}

// cardEvent mirrors the event.* block of card.action.trigger.
type cardEvent struct {
	Operator struct {
		OpenID string `json:"open_id"`
	} `json:"operator"`
	Action struct {
		Value     map[string]any `json:"value"`
		FormValue map[string]any `json:"form_value"`
	} `json:"action"`
	Context struct {
		OpenMessageID string `json:"open_message_id"`
		OpenChatID    string `json:"open_chat_id"`
	} `json:"context"`
}

// parseCardAction builds the ws-level CardAction from raw JSON.
func parseCardAction(eventID string, raw json.RawMessage) (*CardAction, error) {
	var ce cardEvent
	if err := json.Unmarshal(raw, &ce); err != nil {
		return nil, fmt.Errorf("ws: parse card event: %w", err)
	}
	return &CardAction{
		EventID:   eventID,
		ChatID:    ce.Context.OpenChatID,
		MessageID: ce.Context.OpenMessageID,
		Operator:  CardActionOperator{OpenID: ce.Operator.OpenID},
		Action: CardActionPayload{
			Value:     ce.Action.Value,
			FormValue: ce.Action.FormValue,
		},
	}, nil
}
