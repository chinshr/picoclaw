package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
)

// recordingBus is a minimal interfaces.MessageBus for intake tests.
type recordingBus struct {
	mu       sync.Mutex
	outbound []bus.OutboundMessage
	inbound  []bus.InboundMessage
	ch       chan bus.InboundMessage
}

func newRecordingBus() *recordingBus {
	return &recordingBus{ch: make(chan bus.InboundMessage, 16)}
}

func (b *recordingBus) PublishInbound(_ context.Context, msg bus.InboundMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inbound = append(b.inbound, msg)
	return nil
}

func (b *recordingBus) PublishOutbound(_ context.Context, msg bus.OutboundMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.outbound = append(b.outbound, msg)
	return nil
}

func (b *recordingBus) PublishOutboundMedia(_ context.Context, _ bus.OutboundMediaMessage) error {
	return nil
}

func (b *recordingBus) GetStreamer(_ context.Context, _, _, _ string) (bus.Streamer, bool) {
	return nil, false
}

func (b *recordingBus) InboundChan() <-chan bus.InboundMessage { return b.ch }

func (b *recordingBus) outboundSnapshot() []bus.OutboundMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bus.OutboundMessage(nil), b.outbound...)
}

func (b *recordingBus) inboundSnapshot() []bus.InboundMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bus.InboundMessage(nil), b.inbound...)
}

func intakeTestLoop(t *testing.T, url string) (*AgentLoop, *recordingBus) {
	t.Helper()
	msgBus := newRecordingBus()
	al := &AgentLoop{
		bus: msgBus,
		cfg: &config.Config{
			Hooks: config.HooksConfig{
				Intake: config.IntakeHookConfig{
					Enabled:  true,
					URL:      url,
					Channels: []string{"telegram"},
				},
			},
		},
	}
	al.intake = newIntakeDispatcher(al)
	return al, msgBus
}

func photoMessage(id, text string) bus.InboundMessage {
	return bus.InboundMessage{
		Context: bus.InboundContext{
			Channel:   "telegram",
			ChatID:    "chat-1",
			MessageID: id,
		},
		Channel: "telegram",
		ChatID:  "chat-1",
		Content: text,
		Media:   []string{"media://" + id},
	}
}

// The bug this whole hook exists for: three covers sent back to back must
// produce three independent intake calls, in order. Through the agent path they
// become one turn and one reply.
func TestIntakeDispatcher_BurstIsHandledIndependentlyAndInOrder(t *testing.T) {
	var mu sync.Mutex
	var seen []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req intakeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		seen = append(seen, req.Text)
		count := len(seen)
		mu.Unlock()
		// Slow enough that a serialized-by-session implementation would show up
		// as merged calls rather than three.
		time.Sleep(10 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(intakeResponse{
			Handled: true,
			Reply:   "queued for check-in (#" + string(rune('0'+count)) + ")",
		})
	}))
	defer srv.Close()

	al, msgBus := intakeTestLoop(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	al.intake.Start(ctx)

	titles := []string{"Vacation Under the Volcano", "How It Happens", "The Winter Soldier"}
	for i, title := range titles {
		msg := photoMessage(string(rune('a'+i)), title)
		if !al.intake.ShouldHandle(msg) {
			t.Fatalf("message %d should be handled by intake", i)
		}
		if !al.intake.Enqueue(msg) {
			t.Fatalf("message %d should enqueue", i)
		}
	}

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == len(titles)
	}, "three intake calls")

	mu.Lock()
	defer mu.Unlock()
	for i, title := range titles {
		if seen[i] != title {
			t.Errorf("intake %d: got %q, want %q (submission order must be preserved)", i, seen[i], title)
		}
	}

	waitFor(t, func() bool { return len(msgBus.outboundSnapshot()) == 3 }, "three replies")
	for _, out := range msgBus.outboundSnapshot() {
		if out.Context.Raw[metadataKeyOutboundKind] != outboundKindFinal {
			t.Errorf("intake reply must be marked final so its placeholder is resolved, raw=%v",
				out.Context.Raw)
		}
	}
	if got := len(msgBus.inboundSnapshot()); got != 0 {
		t.Errorf("handled messages must not be returned to the agent, got %d", got)
	}
}

// A declined message goes back to the agent, marked so it is not offered twice.
func TestIntakeDispatcher_DeclinedMessageFallsBackToAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(intakeResponse{Handled: false, Error: "not a check-in"})
	}))
	defer srv.Close()

	al, msgBus := intakeTestLoop(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	al.intake.Start(ctx)

	al.intake.Enqueue(photoMessage("m1", "what is this book about?"))

	waitFor(t, func() bool { return len(msgBus.inboundSnapshot()) == 1 }, "message returned to agent")
	returned := msgBus.inboundSnapshot()[0]
	if returned.Context.Raw[RawKeyIntakeSeen] != "1" {
		t.Errorf("returned message must be marked seen, raw=%v", returned.Context.Raw)
	}
	if al.intake.ShouldHandle(returned) {
		t.Error("a returned message must not be offered to intake again")
	}
	if got := len(msgBus.outboundSnapshot()); got != 0 {
		t.Errorf("declined message must not produce a reply, got %d", got)
	}
}

// A dead endpoint must degrade to the old behaviour, never swallow the message.
func TestIntakeDispatcher_EndpointErrorFallsBackToAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	al, msgBus := intakeTestLoop(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	al.intake.Start(ctx)

	al.intake.Enqueue(photoMessage("m1", "checkin"))

	waitFor(t, func() bool { return len(msgBus.inboundSnapshot()) == 1 }, "message returned to agent")
}

func TestIntakeDispatcher_ShouldHandleGating(t *testing.T) {
	al, _ := intakeTestLoop(t, "http://127.0.0.1:1/intake")

	textOnly := photoMessage("m1", "how many books do we have?")
	textOnly.Media = nil
	if al.intake.ShouldHandle(textOnly) {
		t.Error("text-only message must stay on the agent path by default")
	}

	otherChannel := photoMessage("m2", "checkin")
	otherChannel.Channel = "discord"
	if al.intake.ShouldHandle(otherChannel) {
		t.Error("channel outside hooks.intake.channels must not be intercepted")
	}

	al.cfg.Hooks.Intake.Enabled = false
	if al.intake.ShouldHandle(photoMessage("m3", "checkin")) {
		t.Error("disabled hook must not intercept")
	}
	al.cfg.Hooks.Intake.Enabled = true
	if !al.intake.ShouldHandle(photoMessage("m4", "checkin")) {
		t.Error("re-enabling the hook must take effect without a restart")
	}
}

// Attachments reach the endpoint as readable local paths, and a ref whose file
// is gone is flagged rather than silently sent as a path that cannot be opened.
func TestIntakeDispatcher_BuildRequestResolvesAttachments(t *testing.T) {
	store := media.NewFileMediaStore()
	dir := t.TempDir()
	path := filepath.Join(dir, "cover.png")
	if err := os.WriteFile(path, []byte("cover-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref, err := store.Store(path, media.MediaMeta{Filename: "cover.png"}, "test")
	if err != nil {
		t.Fatal(err)
	}

	al, _ := intakeTestLoop(t, "http://127.0.0.1:1/intake")
	al.mediaStore = store

	msg := photoMessage("m1", "checkin")
	msg.Media = []string{ref, "media://never-stored"}
	req := al.intake.buildRequest(msg)

	if len(req.Attachments) != 2 {
		t.Fatalf("expected 2 attachments, got %d", len(req.Attachments))
	}
	if req.Attachments[0].Path != path {
		t.Errorf("expected local path %q, got %q", path, req.Attachments[0].Path)
	}
	if req.Attachments[0].Missing {
		t.Error("a readable attachment must not be flagged missing")
	}
	if req.Attachments[0].Bytes != int64(len("cover-bytes")) {
		t.Errorf("unexpected size %d", req.Attachments[0].Bytes)
	}
	if !req.Attachments[1].Missing {
		t.Error("an unresolvable ref must be flagged missing")
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
