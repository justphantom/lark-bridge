package agnesback

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// fakeClient implements APIClient: it records calls and returns canned results.
type fakeClient struct {
	mu sync.Mutex

	promptOut string
	promptErr error

	imgOut  []byte
	imgMime string
	imgErr  error

	videoOut      string
	videoErr      error
	videoProgress []int // progress values passed to pollCB, for assertions

	videoData  []byte // bytes returned by DownloadVideo; nil → error
	videoDlErr error

	promptCalls int
	imageCalls  int
	videoCalls  int
}

func (f *fakeClient) GeneratePrompt(_ context.Context, _, _ string) (string, error) {
	f.mu.Lock()
	f.promptCalls++
	f.mu.Unlock()
	return f.promptOut, f.promptErr
}

func (f *fakeClient) GenerateImage(_ context.Context, _ string) ([]byte, string, error) {
	f.mu.Lock()
	f.imageCalls++
	f.mu.Unlock()
	return f.imgOut, f.imgMime, f.imgErr
}

func (f *fakeClient) GenerateVideo(_ context.Context, _ string, pollCB func(string, int)) (string, error) {
	f.mu.Lock()
	f.videoCalls++
	f.mu.Unlock()
	// Simulate a couple of progress ticks before returning.
	for _, p := range f.videoProgress {
		if pollCB != nil {
			pollCB("in_progress", p)
		}
	}
	return f.videoOut, f.videoErr
}

func (f *fakeClient) DownloadVideo(_ context.Context, _ string) ([]byte, error) {
	if f.videoDlErr != nil {
		return nil, f.videoDlErr
	}
	return f.videoData, nil
}

// fakeSender captures SendControl calls under a mutex.
type fakeSender struct {
	mu        sync.Mutex
	captured  []*protocol.Control
	notifyErr error
}

func (f *fakeSender) SendControl(_ context.Context, ctrl *protocol.Control) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	clone := *ctrl
	f.captured = append(f.captured, &clone)
	return f.notifyErr
}

func (f *fakeSender) snapshot() []*protocol.Control {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*protocol.Control, len(f.captured))
	copy(out, f.captured)
	return out
}

func (f *fakeSender) waitTerminal(t *testing.T, n int) []*protocol.Control {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := f.snapshot()
		if countTerminal(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d terminal controls, got %d (%+v)", n, countTerminal(got), got)
		}
		time.Sleep(time.Millisecond)
	}
}

func countTerminal(ctrls []*protocol.Control) int {
	n := 0
	for _, c := range ctrls {
		if c.Type == protocol.TypeNotice || c.Type == protocol.TypeFile || c.Type == protocol.TypeResult {
			n++
		}
	}
	return n
}

func newTestHandler(fc APIClient) (*Handler, *fakeSender) {
	s := &fakeSender{}
	h := NewHandler(Config{
		ChatModel:   "agnes-2.5-flash",
		ImageModel:  "agnes-image-2.1-flash",
		VideoModel:  "agnes-video-v2.0",
		ImageSize:   "1K",
		ImageRatio:  "1:1",
		ChatModels:  []string{"agnes-2.5-flash", "agnes-2.5-pro"},
		ImageModels: []string{"agnes-image-2.1-flash", "agnes-image-2.2"},
		VideoModels: []string{"agnes-video-v2.0", "agnes-video-v3.0"},
	}, fc, s, nil)
	return h, s
}

func promptEvent(text string) *protocol.Event {
	return &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "p1",
		ChatID:   "oc_test",
		Prompt:   &protocol.PromptPayload{ChatID: "oc_test", Text: text},
	}
}

func TestHandleEvent_ImagePrompt_OK(t *testing.T) {
	fc := &fakeClient{promptOut: "a luminous city"}
	h, s := newTestHandler(fc)
	if err := h.HandleEvent(context.Background(), promptEvent("/image-prompt 浮空城市")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	got := s.waitTerminal(t, 1)
	last := got[len(got)-1]
	if last.Type != protocol.TypeNotice {
		t.Fatalf("expected terminal notice, got %s", last.Type)
	}
	if !strings.Contains(last.Notice.Message, "luminous city") {
		t.Fatalf("notice message = %q, want the prompt", last.Notice.Message)
	}
}

func TestHandleEvent_ImagePrompt_Error(t *testing.T) {
	fc := &fakeClient{promptErr: errors.New("boom")}
	h, s := newTestHandler(fc)
	_ = h.HandleEvent(context.Background(), promptEvent("/image-prompt x"))
	got := s.waitTerminal(t, 1)
	last := got[len(got)-1]
	if last.Type != protocol.TypeNotice || last.Notice.Level != "error" {
		t.Fatalf("expected error notice, got %+v", last)
	}
}

func TestHandleEvent_Image_OK(t *testing.T) {
	fc := &fakeClient{imgOut: []byte{1, 2, 3}, imgMime: "image/png"}
	h, s := newTestHandler(fc)
	_ = h.HandleEvent(context.Background(), promptEvent("/image 一只猫"))
	got := s.waitTerminal(t, 1)
	// Image should land as a TypeFile control carrying base64 content.
	var file *protocol.Control
	for _, c := range got {
		if c.Type == protocol.TypeFile {
			file = c
			break
		}
	}
	if file == nil {
		t.Fatalf("no TypeFile control emitted; got %+v", got)
	}
	if file.File.FileName == "" || !strings.HasSuffix(file.File.FileName, ".png") {
		t.Fatalf("file name = %q", file.File.FileName)
	}
	if file.File.MIMEType != "image/png" {
		t.Fatalf("mime = %q", file.File.MIMEType)
	}
}

func TestHandleEvent_Image_TooLarge_FallsBackToURL(t *testing.T) {
	// When the image bytes exceed the Feishu 30 MiB cap, GenerateImage is
	// unreachable (the client caps the download); instead the handler relies on
	// the client returning an error, which surfaces as an error notice.
	fc := &fakeClient{imgErr: errors.New("agnes: image exceeds 31457280 bytes")}
	h, s := newTestHandler(fc)
	_ = h.HandleEvent(context.Background(), promptEvent("/image big"))
	got := s.waitTerminal(t, 1)
	last := got[len(got)-1]
	if last.Type != protocol.TypeNotice || last.Notice.Level != "error" {
		t.Fatalf("expected error notice for oversized image, got %+v", last)
	}
}

func TestHandleEvent_Video_OK(t *testing.T) {
	fc := &fakeClient{
		videoOut:      "https://example.com/v.mp4",
		videoProgress: []int{10, 50, 100},
		videoData:     []byte{0x00, 0x00, 0x00, 0x18, 0x66, 0x74, 0x79, 0x70}, // fake mp4 header
	}
	h, s := newTestHandler(fc)
	_ = h.HandleEvent(context.Background(), promptEvent("/video 一只猫在跑"))
	got := s.waitTerminal(t, 1)
	// Video downloaded successfully → TypeFile (inline file in chat).
	var file *protocol.Control
	for _, c := range got {
		if c.Type == protocol.TypeFile {
			file = c
			break
		}
	}
	if file == nil {
		t.Fatalf("no TypeFile control emitted; got %+v", got)
	}
	if file.File.FileName == "" || !strings.HasSuffix(file.File.FileName, ".mp4") {
		t.Fatalf("file name = %q", file.File.FileName)
	}
	if file.File.MIMEType != "video/mp4" {
		t.Fatalf("mime = %q", file.File.MIMEType)
	}
}

// TestHandleEvent_Video_DownloadFails_FallsBackToURL verifies that when the
// video download fails (e.g. exceeds 30 MiB or CDN unreachable), the handler
// falls back to delivering the video URL as a notice so the user still gets
// the result.
func TestHandleEvent_Video_DownloadFails_FallsBackToURL(t *testing.T) {
	fc := &fakeClient{
		videoOut:      "https://example.com/v.mp4",
		videoProgress: []int{100},
		videoData:     nil,
		videoDlErr:    errors.New("agnes: video exceeds 31457280 bytes"),
	}
	h, s := newTestHandler(fc)
	_ = h.HandleEvent(context.Background(), promptEvent("/video big"))
	got := s.waitTerminal(t, 1)
	last := got[len(got)-1]
	if last.Type != protocol.TypeNotice {
		t.Fatalf("expected fallback notice, got %s", last.Type)
	}
	if last.Notice.Level != "success" || !strings.Contains(last.Notice.Message, "example.com/v.mp4") {
		t.Fatalf("fallback notice = %+v", last.Notice)
	}
}

func TestHandleEvent_Video_Error(t *testing.T) {
	fc := &fakeClient{videoErr: errors.New("agnes: video generation failed")}
	h, s := newTestHandler(fc)
	_ = h.HandleEvent(context.Background(), promptEvent("/video fail"))
	got := s.waitTerminal(t, 1)
	last := got[len(got)-1]
	if last.Type != protocol.TypeNotice || last.Notice.Level != "error" {
		t.Fatalf("expected error notice, got %+v", last)
	}
}

func TestHandleEvent_UnknownCommand(t *testing.T) {
	fc := &fakeClient{}
	h, s := newTestHandler(fc)
	// Unknown command: the handler emits a terminal usage notice so the turn
	// does not orphan a "处理中" card.
	_ = h.HandleEvent(context.Background(), promptEvent("/nonsense"))
	got := s.waitTerminal(t, 1)
	last := got[len(got)-1]
	if last.Type != protocol.TypeNotice {
		t.Fatalf("expected terminal notice for unknown command, got %+v", last)
	}
}

func TestHandleEvent_EmptyArg(t *testing.T) {
	fc := &fakeClient{}
	h, s := newTestHandler(fc)
	// A bare /image with no argument should be rejected as a usage notice, not
	// forwarded to the API with an empty prompt.
	_ = h.HandleEvent(context.Background(), promptEvent("/image"))
	got := s.waitTerminal(t, 1)
	if fc.imageCalls != 0 {
		t.Fatalf("API called with empty prompt; imageCalls=%d", fc.imageCalls)
	}
	last := got[len(got)-1]
	if last.Type != protocol.TypeNotice || last.Notice.Level != "error" {
		t.Fatalf("expected error notice for empty arg, got %+v", last)
	}
}

func TestHandleEvent_NonPrompt_Ignored(t *testing.T) {
	fc := &fakeClient{}
	h, s := newTestHandler(fc)
	// A non-prompt, non-ping event (e.g. Abort) must be silently ignored.
	if err := h.HandleEvent(context.Background(), &protocol.Event{Type: protocol.TypeAbort}); err != nil {
		t.Fatalf("HandleEvent abort: %v", err)
	}
	if len(s.snapshot()) != 0 {
		t.Fatalf("non-prompt event emitted controls: %+v", s.snapshot())
	}
}

func TestHandleEvent_PingAnswersPong(t *testing.T) {
	fc := &fakeClient{}
	h, s := newTestHandler(fc)
	// The frontend's C2 health probe must be answered with a TypePong,
	// otherwise the backend is evicted after maxMissedPongs.
	if err := h.HandleEvent(context.Background(), &protocol.Event{Type: protocol.TypePing}); err != nil {
		t.Fatalf("HandleEvent ping: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := s.snapshot()
		if len(got) == 1 && got[0].Type == protocol.TypePong && got[0].Pong != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected one TypePong control, got %+v", got)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHandleEvent_Help(t *testing.T) {
	fc := &fakeClient{}
	h, s := newTestHandler(fc)
	_ = h.HandleEvent(context.Background(), promptEvent("/help"))
	got := s.waitTerminal(t, 1)
	last := got[len(got)-1]
	if last.Type != protocol.TypeNotice || !strings.Contains(last.Notice.Message, "/image-prompt") {
		t.Fatalf("help notice = %+v", last.Notice)
	}
	if !strings.Contains(last.Notice.Message, "/model") {
		t.Fatalf("help should list /model, got %q", last.Notice.Message)
	}
}

func TestHandleEvent_ModelSetAndClear(t *testing.T) {
	fc := &fakeClient{}
	h, s := newTestHandler(fc)

	// Direct set.
	_ = h.HandleEvent(context.Background(), promptEvent("/model image agnes-image-2.2"))
	got := s.waitTerminal(t, 1)
	if n := got[len(got)-1].Notice; n == nil || n.Level != "success" ||
		!strings.Contains(n.Message, "agnes-image-2.2") {
		t.Fatalf("set notice = %+v", got[len(got)-1].Notice)
	}
	eff, overridden := h.effectiveModels()
	if eff[ModelSlotImage] != "agnes-image-2.2" || !overridden[ModelSlotImage] {
		t.Fatalf("eff = %v overridden = %v", eff, overridden)
	}

	// Unknown model.
	_ = h.HandleEvent(context.Background(), promptEvent("/model chat unknown-model"))
	got = s.waitTerminal(t, 2)
	if n := got[len(got)-1].Notice; n == nil || n.Level != "error" {
		t.Fatalf("unknown-model notice = %+v", got[len(got)-1].Notice)
	}

	// Bad slot.
	_ = h.HandleEvent(context.Background(), promptEvent("/model bogus x"))
	got = s.waitTerminal(t, 3)
	if n := got[len(got)-1].Notice; n == nil || n.Level != "error" {
		t.Fatalf("bad-slot notice = %+v", got[len(got)-1].Notice)
	}

	// Clear one slot.
	_ = h.HandleEvent(context.Background(), promptEvent("/model image clear"))
	got = s.waitTerminal(t, 4)
	if n := got[len(got)-1].Notice; n == nil || n.Level != "success" {
		t.Fatalf("clear notice = %+v", got[len(got)-1].Notice)
	}
	eff, overridden = h.effectiveModels()
	if eff[ModelSlotImage] != "agnes-image-2.1-flash" || overridden[ModelSlotImage] {
		t.Fatalf("after clear eff = %v overridden = %v", eff, overridden)
	}
}

func TestHandleEvent_ModelClearAll(t *testing.T) {
	fc := &fakeClient{}
	h, s := newTestHandler(fc)
	h.setModelOverride(ModelSlotChat, "agnes-2.5-pro")
	h.setModelOverride(ModelSlotVideo, "agnes-video-v3.0")

	_ = h.HandleEvent(context.Background(), promptEvent("/model clear"))
	got := s.waitTerminal(t, 1)
	if n := got[len(got)-1].Notice; n == nil || n.Level != "success" || !strings.Contains(n.Message, "已清除全部模型覆盖") {
		t.Fatalf("clear-all notice = %+v", got[len(got)-1].Notice)
	}
	_, overridden := h.effectiveModels()
	if overridden[ModelSlotChat] || overridden[ModelSlotImage] || overridden[ModelSlotVideo] {
		t.Fatalf("overrides should be cleared, got %v", overridden)
	}
}

func TestHandleEvent_ModelUsage(t *testing.T) {
	fc := &fakeClient{}
	h, s := newTestHandler(fc)
	_ = h.HandleEvent(context.Background(), promptEvent("/model chat"))
	got := s.waitTerminal(t, 1)
	if n := got[len(got)-1].Notice; n == nil || n.Level != "error" || !strings.Contains(n.Message, "用法") {
		t.Fatalf("usage notice = %+v", got[len(got)-1].Notice)
	}
}

// TestHandleEvent_ModelPicker drives the full interactive flow: /model emits a
// three-question card; a delivered Answer applies the changed slots and patches
// the card in place via UpdateMessageID.
func TestHandleEvent_ModelPicker(t *testing.T) {
	fc := &fakeClient{}
	h, s := newTestHandler(fc)

	// Fire /model (no args) — it spawns the picker goroutine.
	_ = h.HandleEvent(context.Background(), promptEvent("/model"))

	// Wait for the question card.
	var q *protocol.Control
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, c := range s.snapshot() {
			if c.Type == protocol.TypeQuestion {
				q = c
			}
		}
		if q != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for question card")
		}
		time.Sleep(time.Millisecond)
	}
	if len(q.Question.Questions) != 3 {
		t.Fatalf("want 3 questions, got %d", len(q.Question.Questions))
	}
	if !q.Question.TakeOverProgress {
		t.Fatal("question should take over the progress card")
	}
	// First option of each question is the keep-current entry.
	for i, item := range q.Question.Questions {
		if len(item.Options) < 2 || !strings.HasPrefix(item.Options[0], modelKeepPrefix) {
			t.Fatalf("question %d options = %v", i, item.Options)
		}
	}

	// Deliver an answer: change chat + video, keep image.
	ans := &protocol.AnswerPayload{
		ChatID:    "oc_test",
		RequestID: q.Question.RequestID,
		MessageID: "om_picker",
		Choices:   []string{"agnes-2.5-pro", q.Question.Questions[1].Options[0], "agnes-video-v3.0"},
	}
	_ = h.HandleEvent(context.Background(), &protocol.Event{
		Type:   protocol.TypeAnswer,
		ChatID: "oc_test",
		Answer: ans,
	})

	// Wait for the result card patch.
	deadline = time.Now().Add(2 * time.Second)
	var res *protocol.Control
	for {
		for _, c := range s.snapshot() {
			if c.Type == protocol.TypeNotice && c.Notice != nil && c.Notice.UpdateMessageID == "om_picker" {
				res = c
			}
		}
		if res != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for result card patch")
		}
		time.Sleep(time.Millisecond)
	}
	if res.Notice.Title != "已切换模型" ||
		!strings.Contains(res.Notice.Message, "chat：agnes-2.5-flash → agnes-2.5-pro") ||
		!strings.Contains(res.Notice.Message, "video：agnes-video-v2.0 → agnes-video-v3.0") {
		t.Fatalf("result card = %+v", res.Notice)
	}
	eff, overridden := h.effectiveModels()
	if eff[ModelSlotChat] != "agnes-2.5-pro" || eff[ModelSlotVideo] != "agnes-video-v3.0" ||
		eff[ModelSlotImage] != "agnes-image-2.1-flash" || overridden[ModelSlotImage] {
		t.Fatalf("eff = %v overridden = %v", eff, overridden)
	}
}

// TestHandleEvent_ModelPickerUnchanged verifies the all-keep answer reports no
// change and installs no overrides.
func TestHandleEvent_ModelPickerUnchanged(t *testing.T) {
	fc := &fakeClient{}
	h, s := newTestHandler(fc)
	_ = h.HandleEvent(context.Background(), promptEvent("/model"))

	var q *protocol.Control
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, c := range s.snapshot() {
			if c.Type == protocol.TypeQuestion {
				q = c
			}
		}
		if q != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for question card")
		}
		time.Sleep(time.Millisecond)
	}

	choices := make([]string, 3)
	for i, item := range q.Question.Questions {
		choices[i] = item.Options[0] // keep current
	}
	_ = h.HandleEvent(context.Background(), &protocol.Event{
		Type:   protocol.TypeAnswer,
		ChatID: "oc_test",
		Answer: &protocol.AnswerPayload{
			ChatID: "oc_test", RequestID: q.Question.RequestID, MessageID: "om_k", Choices: choices,
		},
	})

	deadline = time.Now().Add(2 * time.Second)
	for {
		found := false
		for _, c := range s.snapshot() {
			if c.Type == protocol.TypeNotice && c.Notice != nil && c.Notice.UpdateMessageID == "om_k" {
				if c.Notice.Title != "模型未变化" {
					t.Fatalf("title = %q, want 模型未变化", c.Notice.Title)
				}
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for no-change card")
		}
		time.Sleep(time.Millisecond)
	}
	if _, overridden := h.effectiveModels(); overridden[ModelSlotChat] || overridden[ModelSlotImage] || overridden[ModelSlotVideo] {
		t.Fatalf("no overrides expected, got %v", overridden)
	}
}
