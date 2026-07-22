package feishu

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	sdktypes "github.com/larksuite/oapi-sdk-go/v3/channel/types"

	"github.com/justphantom/lark-bridge/internal/log"
)

// withChannelFactory routes freshChannel through a fake factory instead of
// newChannel. Test-only BotOption; production never wires it.
func withChannelFactory(fn func() sdktypes.Channel) BotOption {
	return func(c *botConfig) { c.channelFactory = fn }
}

// setCh injects a channel into b.ch via the same atomic.Pointer production
// code uses (NewBotWithLogger does Store(&ch); tests mirror that here).
func setCh(b *Bot, ch sdktypes.Channel) { b.ch.Store(&ch) }

// recordingChannel is a fakeChannel with thread-safe counters. startGate
// makes Start observable: closing it lets the goroutine Restart spawns
// return, so -race tests synchronize on startCount without a fixed sleep.
type recordingChannel struct {
	startCount atomic.Int32
	stopCount  atomic.Int32

	onCardActionCnt atomic.Int32
	onReadyCnt      atomic.Int32
	onErrorCnt      atomic.Int32
	onReconnecting  atomic.Int32
	onReconnected   atomic.Int32
	onDisconnected  atomic.Int32

	startGate chan struct{}
}

func newRecordingChannel() *recordingChannel {
	return &recordingChannel{startGate: make(chan struct{})}
}

func (r *recordingChannel) releaseStart() {
	select {
	case <-r.startGate:
	default:
		close(r.startGate)
	}
}

func (r *recordingChannel) Send(_ context.Context, _ *sdktypes.SendInput) (*sdktypes.SendResult, error) {
	return nil, errors.New("recordingChannel: Send not used")
}
func (r *recordingChannel) OnMessage(func(context.Context, *sdktypes.NormalizedMessage) error) {}
func (r *recordingChannel) OnReaction(func(context.Context, *sdktypes.ReactionEvent) error)    {}
func (r *recordingChannel) OnComment(func(context.Context, *sdktypes.CommentEvent) error)      {}
func (r *recordingChannel) OnBotAdded(func(context.Context, *sdktypes.BotAddedEvent) error)    {}
func (r *recordingChannel) OnCardAction(func(context.Context, *sdktypes.CardActionEvent) error) {
	r.onCardActionCnt.Add(1)
}
func (r *recordingChannel) OnReject(func(context.Context, *sdktypes.RejectEvent) error) {}
func (r *recordingChannel) DownloadFile(context.Context, string, string) ([]byte, error) {
	return nil, nil
}
func (r *recordingChannel) OnReady(func())        { r.onReadyCnt.Add(1) }
func (r *recordingChannel) OnError(func(error))   { r.onErrorCnt.Add(1) }
func (r *recordingChannel) OnReconnecting(func()) { r.onReconnecting.Add(1) }
func (r *recordingChannel) OnReconnected(func())  { r.onReconnected.Add(1) }
func (r *recordingChannel) OnDisconnected(func()) { r.onDisconnected.Add(1) }
func (r *recordingChannel) Start(ctx context.Context) error {
	r.startCount.Add(1)
	select {
	case <-r.startGate:
	case <-ctx.Done():
	}
	return nil
}
func (r *recordingChannel) Stream(context.Context, *sdktypes.SendInput) (sdktypes.StreamController, error) {
	return nil, nil //nolint:nilnil // sdk contract: (nil, nil) signals "no stream supported"
}
func (r *recordingChannel) UpdatePolicy(sdktypes.PolicyConfig)                   {}
func (r *recordingChannel) GetPolicy() sdktypes.PolicyConfig                     { return sdktypes.PolicyConfig{} }
func (r *recordingChannel) GetBotIdentity(context.Context) *sdktypes.BotIdentity { return nil }
func (r *recordingChannel) Stop(context.Context) error {
	r.stopCount.Add(1)
	return nil
}

// waitForInt32 polls c until it reaches want or timeout. Synchronizes on
// the `go Start` goroutine Restart spawns.
func waitForInt32(c *atomic.Int32, want int32, timeout time.Duration) int32 {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := c.Load(); got >= want {
			return got
		}
		time.Sleep(time.Millisecond)
	}
	return c.Load()
}

// assertHandlersMounted fails t if any of the six callbacks was not registered.
func assertHandlersMounted(t *testing.T, r *recordingChannel) {
	t.Helper()
	cases := []struct {
		name string
		got  int32
	}{
		{"OnCardAction", r.onCardActionCnt.Load()},
		{"OnReady", r.onReadyCnt.Load()},
		{"OnError", r.onErrorCnt.Load()},
		{"OnReconnecting", r.onReconnecting.Load()},
		{"OnReconnected", r.onReconnected.Load()},
		{"OnDisconnected", r.onDisconnected.Load()},
	}
	for _, c := range cases {
		if c.got != 1 {
			t.Errorf("%s registrations = %d, want 1", c.name, c.got)
		}
	}
}

// TestRestart_SwapsChannel verifies Restart's core contract: a fresh channel
// becomes the new Load() target, handlers re-mount on it, its Start runs in
// a goroutine, and the old channel is Stopped exactly once.
func TestRestart_SwapsChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	old := newRecordingChannel()
	old.releaseStart()
	b := &Bot{logger: log.Nop()}
	setCh(b, old)
	oldStartsBefore := old.startCount.Load()

	fresh := newRecordingChannel()
	b.newChannelFn = func() sdktypes.Channel { return fresh }

	if err := b.Restart(ctx); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if got := waitForInt32(&fresh.startCount, 1, time.Second); got != 1 {
		t.Fatalf("fresh.Start count = %d, want 1", got)
	}
	if got := old.stopCount.Load(); got != 1 {
		t.Errorf("old.Stop count = %d, want 1", got)
	}
	if got := old.startCount.Load(); got != oldStartsBefore {
		t.Errorf("old.Start count drifted: was %d now %d", oldStartsBefore, got)
	}
	loaded := b.ch.Load()
	if loaded == nil || *loaded != sdktypes.Channel(fresh) {
		t.Errorf("b.ch.Load() = %v, want fresh %p", loaded, fresh)
	}
	assertHandlersMounted(t, fresh)
}

// TestRestart_NoOldChannelNoStop verifies Restart tolerates a nil prior
// channel: it must skip the (*old).Stop call and not panic.
func TestRestart_NoOldChannelNoStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := &Bot{logger: log.Nop()} // b.ch zero → Load() returns nil
	fresh := newRecordingChannel()
	b.newChannelFn = func() sdktypes.Channel { return fresh }

	if err := b.Restart(ctx); err != nil {
		t.Fatalf("Restart with no prior channel: %v", err)
	}
	if got := waitForInt32(&fresh.startCount, 1, time.Second); got != 1 {
		t.Fatalf("fresh.Start count = %d, want 1", got)
	}
	loaded := b.ch.Load()
	if loaded == nil || *loaded != sdktypes.Channel(fresh) {
		t.Errorf("b.ch.Load() = %v, want fresh %p", loaded, fresh)
	}
}

// TestNewBotWithLogger_UsesNewChannel verifies the constructor stores a
// channel, wires larkClient/imService, and that registerHandlersOn mounts
// every lifecycle callback. Uses withChannelFactory so OnXXX counts are
// observable (the real SDK channel hides its internals).
func TestNewBotWithLogger_UsesNewChannel(t *testing.T) {
	fake := newRecordingChannel()
	b, err := NewBotWithLogger("fake_app_id", "fake_secret", log.Nop(),
		withChannelFactory(func() sdktypes.Channel { return fake }),
	)
	if err != nil {
		t.Fatalf("NewBotWithLogger: %v", err)
	}
	loaded := b.ch.Load()
	if loaded == nil || *loaded != sdktypes.Channel(fake) {
		t.Errorf("b.ch.Load() = %v, want fake %p", loaded, fake)
	}
	if b.larkClient == nil {
		t.Error("b.larkClient = nil, want initialized")
	}
	if b.imService == nil {
		t.Error("b.imService = nil, want initialized")
	}
	assertHandlersMounted(t, fake)
}

// TestStart_UsesLoad verifies Start reads the channel via b.ch.Load() (not a
// stale direct field). With a fakeChannel injected, its Start must be called
// exactly once.
func TestStart_UsesLoad(t *testing.T) {
	fake := newRecordingChannel()
	fake.releaseStart()
	b := &Bot{logger: log.Nop()}
	setCh(b, fake)

	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := fake.startCount.Load(); got != 1 {
		t.Errorf("fake.Start count = %d, want 1", got)
	}
}

// TestRegisterHandlersOn_MountsAllCallbacks covers registerHandlersOn in
// isolation so the "handlers re-mount on each Restart" guarantee is unit-
// testable apart from the Restart flow.
func TestRegisterHandlersOn_MountsAllCallbacks(t *testing.T) {
	fake := newRecordingChannel()
	b := &Bot{logger: log.Nop()}
	b.registerHandlersOn(fake)
	assertHandlersMounted(t, fake)
}
