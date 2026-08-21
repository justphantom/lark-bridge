package feishufront

import (
	"context"
	"sync"

	"github.com/justphantom/lark-bridge/internal/feishu"
)

// Test double shared by the frontend's in-package tests: a CardSink that
// records every SendCard/UpdateCard call and returns synthetic message ids
// so the dispatcher can track turns. (Extracted from interactive_e2e_test.go
// when the e2e suite moved to internal/feishufront/ipcserver; that package
// carries its own copy.)

// fakeSink is a CardSink that records every SendCard/UpdateCard call and
// returns synthetic message ids so the dispatcher can track turns.
type fakeSink struct {
	mu        sync.Mutex
	sends     []sentCard
	textSends []sentText
	updates   []updatedCard
	nextID    int
	cardErr   error // if set, SendCard returns it (simulates a Feishu rejection)
	updateErr error // if set, UpdateCard returns it (simulates a withdrawn card)
}

type sentCard struct {
	chatID    string
	card      []byte
	replyToID string
}
type sentText struct {
	chatID    string
	text      string
	replyToID string
}
type updatedCard struct {
	messageID string
	card      []byte
}

func (f *fakeSink) SendCard(_ context.Context, chatID string, card []byte, replyToID string) (feishu.CardRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cardErr != nil {
		return feishu.CardRef{}, f.cardErr
	}
	f.nextID++
	f.sends = append(f.sends, sentCard{chatID: chatID, card: card, replyToID: replyToID})
	return feishu.CardRef{MessageID: "om_" + itoa(f.nextID)}, nil
}

func (f *fakeSink) SendCardInline(_ context.Context, chatID string, card []byte, replyToID string) (feishu.CardRef, error) {
	// Inline cards (interactive cards with callbacks) land here. CardID is
	// empty so the dispatcher knows to use im PATCH for updates.
	ref, err := f.SendCard(context.Background(), chatID, card, replyToID)
	if err != nil {
		return ref, err
	}
	return feishu.CardRef{MessageID: ref.MessageID}, nil // CardID stays empty
}

func (f *fakeSink) UpdateCard(_ context.Context, messageID, _ string, card []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, updatedCard{messageID: messageID, card: card})
	return f.updateErr
}

// UpdateCardVerified records into the same updates slice as UpdateCard: the
// dispatcher-level fakes have no real Feishu to bounce a PATCH off, so they
// treat the verified path as a plain update for assertion purposes. The Bot's
// own verify loop is exercised in internal/feishu.
func (f *fakeSink) UpdateCardVerified(ctx context.Context, messageID, cardID string, card []byte) error {
	return f.UpdateCard(ctx, messageID, cardID, card)
}

// SendText records a plain-text fallback send so tests can assert the
// result-card-rejected fallback delivered the reply as text.
func (f *fakeSink) SendText(_ context.Context, chatID, text, replyToID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.textSends = append(f.textSends, sentText{chatID: chatID, text: text, replyToID: replyToID})
	return "om_" + itoa(f.nextID), nil
}

func (f *fakeSink) lastSendCard() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sends) == 0 {
		return nil
	}
	return f.sends[len(f.sends)-1].card
}

// counts returns the number of recorded SendCard and UpdateCard calls.
func (f *fakeSink) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends), len(f.updates)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
