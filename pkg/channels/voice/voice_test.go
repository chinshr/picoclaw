package voice

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

func newTestVoiceChannel(t *testing.T) *VoiceChannel {
	t.Helper()

	bc := &config.Channel{Type: config.ChannelVoice, Enabled: true}
	cfg := &config.VoiceSettings{}
	cfg.SetToken("test-token")
	ch, err := NewVoiceChannel(bc, cfg, bus.NewMessageBus())
	if err != nil {
		t.Fatalf("NewVoiceChannel: %v", err)
	}

	ch.ctx = context.Background()
	return ch
}

func TestNewVoiceChannel_RequiresToken(t *testing.T) {
	bc := &config.Channel{Type: config.ChannelVoice, Enabled: true}
	cfg := &config.VoiceSettings{}
	if _, err := NewVoiceChannel(bc, cfg, bus.NewMessageBus()); err == nil {
		t.Fatal("expected error when token is empty")
	}
}

func TestWebhookPath(t *testing.T) {
	ch := newTestVoiceChannel(t)
	if got := ch.WebhookPath(); got != "/voice/" {
		t.Fatalf("WebhookPath() = %q, want %q", got, "/voice/")
	}
}

func TestBroadcastToSession_StripsVoicePrefix(t *testing.T) {
	ch := newTestVoiceChannel(t)
	if err := ch.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer ch.Stop(context.Background())

	clientConn, received, cleanup := newTestVoiceWebSocket(t)
	defer cleanup()
	ch.addConnForTest(&voiceConn{id: "conn-1", conn: clientConn, sessionID: "sess-1"})

	if _, err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "voice:sess-1",
		Content: "hi",
	}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	msg := mustReceiveVoiceMessage(t, received)
	if msg.SessionID != "sess-1" {
		t.Fatalf("session_id = %q, want sess-1", msg.SessionID)
	}
	if got := msg.Payload[PayloadKeyContent]; got != "hi" {
		t.Fatalf("content = %#v, want hi", got)
	}
}

// TestSend_MarksFrameAsFinal protects against a regression where the
// non-streaming Send path emitted a ``message.create`` without
// ``payload.final == true``. Streaming-aware consumers (e.g. the
// library-claw bridge) wait for that flag before resolving the turn — if
// it's missing they buffer the content as an in-progress streaming chunk
// and hang waiting for a terminal frame that never arrives. This bites
// every time picoclaw falls back from ChatStream to Chat (e.g. provider
// 429), because the fallback delivers its single reply via bus.Publish ->
// channel.Send instead of through the streamer.
func TestSend_MarksFrameAsFinal(t *testing.T) {
	ch := newTestVoiceChannel(t)
	if err := ch.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer ch.Stop(context.Background())

	clientConn, received, cleanup := newTestVoiceWebSocket(t)
	defer cleanup()
	ch.addConnForTest(&voiceConn{id: "conn-2", conn: clientConn, sessionID: "sess-2"})

	if _, err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "voice:sess-2",
		Content: "Go ahead — I'm listening.",
	}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	msg := mustReceiveVoiceMessage(t, received)
	if msg.Type != TypeMessageCreate {
		t.Fatalf("type = %q, want %q", msg.Type, TypeMessageCreate)
	}
	final, ok := msg.Payload[PayloadKeyFinal].(bool)
	if !ok || !final {
		t.Fatalf("payload.final = %#v, want true (one-shot Send must be terminal)", msg.Payload[PayloadKeyFinal])
	}
	if got := msg.Payload[PayloadKeyContent]; got != "Go ahead — I'm listening." {
		t.Fatalf("content = %#v, want %q", got, "Go ahead — I'm listening.")
	}
	if _, hasID := msg.Payload["message_id"].(string); !hasID {
		t.Fatalf("payload.message_id missing or non-string: %#v", msg.Payload["message_id"])
	}
}

// TestBeginStream_FlushesOnSentenceBoundary is the central behavior test for
// the voice streamer: as soon as a sentence-ender appears in the streaming
// content, the streamer should flush regardless of the throttle window.
func TestBeginStream_FlushesOnSentenceBoundary(t *testing.T) {
	ch := newTestVoiceChannel(t)
	// Use a very large throttle so that without sentence-flush the second
	// update would be suppressed. With sentence-flush, the period in
	// "Hello world." forces it out anyway.
	ch.config.Streaming = config.StreamingConfig{
		Enabled:         true,
		ThrottleSeconds: 60,
		MinGrowthChars:  1000,
	}
	if err := ch.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer ch.Stop(context.Background())

	clientConn, received, cleanup := newTestVoiceWebSocket(t)
	defer cleanup()
	ch.addConnForTest(&voiceConn{id: "conn-1", conn: clientConn, sessionID: "sess-1"})

	streamer, err := ch.BeginStream(context.Background(), "voice:sess-1")
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}

	if err := streamer.Update(context.Background(), "Hello"); err != nil {
		t.Fatalf("Update(first) error = %v", err)
	}
	first := mustReceiveVoiceMessage(t, received)
	if first.Type != TypeMessageCreate {
		t.Fatalf("first type = %q, want %q", first.Type, TypeMessageCreate)
	}
	msgID, _ := first.Payload["message_id"].(string)
	if msgID == "" {
		t.Fatal("first message_id is empty")
	}

	// Second update completes a sentence — must flush even though throttle
	// would otherwise block.
	if err := streamer.Update(context.Background(), "Hello world."); err != nil {
		t.Fatalf("Update(second) error = %v", err)
	}
	second := mustReceiveVoiceMessage(t, received)
	if second.Type != TypeMessageUpdate {
		t.Fatalf("second type = %q, want %q", second.Type, TypeMessageUpdate)
	}
	if got := second.Payload[PayloadKeyContent]; got != "Hello world." {
		t.Fatalf("second content = %#v, want %q", got, "Hello world.")
	}
	if got := second.Payload["message_id"]; got != msgID {
		t.Fatalf("second message_id = %#v, want %q", got, msgID)
	}
}

// TestBeginStream_SuppressedBetweenSentenceBoundaries verifies the fallback
// path: when no new sentence-ender is present, the streamer still respects
// the throttle window — i.e. it doesn't accidentally flush every update.
func TestBeginStream_SuppressedBetweenSentenceBoundaries(t *testing.T) {
	ch := newTestVoiceChannel(t)
	ch.config.Streaming = config.StreamingConfig{
		Enabled:         true,
		ThrottleSeconds: 60,
		MinGrowthChars:  1000,
	}
	if err := ch.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer ch.Stop(context.Background())

	clientConn, received, cleanup := newTestVoiceWebSocket(t)
	defer cleanup()
	ch.addConnForTest(&voiceConn{id: "conn-1", conn: clientConn, sessionID: "sess-1"})

	streamer, err := ch.BeginStream(context.Background(), "voice:sess-1")
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}

	// First Update primes lastSentenceEnd to the "." position.
	if err := streamer.Update(context.Background(), "Hello world."); err != nil {
		t.Fatalf("Update(first) error = %v", err)
	}
	_ = mustReceiveVoiceMessage(t, received)

	// No new sentence-end; throttle is huge; min-growth is huge. Should be
	// suppressed.
	if err := streamer.Update(context.Background(), "Hello world. tail"); err != nil {
		t.Fatalf("Update(second) error = %v", err)
	}
	assertNoVoiceMessage(t, received)

	// Finalize must always flush.
	if err := streamer.Finalize(context.Background(), "Hello world. tail"); err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	final := mustReceiveVoiceMessage(t, received)
	if final.Type != TypeMessageUpdate {
		t.Fatalf("final type = %q, want %q", final.Type, TypeMessageUpdate)
	}
	if got, _ := final.Payload[PayloadKeyFinal].(bool); !got {
		t.Fatalf("final flag = %#v, want true", final.Payload[PayloadKeyFinal])
	}
}

func TestSplitThinking(t *testing.T) {
	cases := []struct {
		in, clean, thinking string
	}{
		{"Take any book home.", "Take any book home.", ""},
		{"<think>they seem confused</think>Here you go.", "Here you go.", "they seem confused"},
		{"<thinking>plan A</thinking>Hi.<thinking>plan B</thinking>", "Hi.", "plan Aplan B"},
		// Unclosed span: fail-closed — everything after the tag is thinking.
		{"Sure.<think>let me consider the shelf", "Sure.", "let me consider the shelf"},
		// Case-insensitive, mixed open/close variants.
		{"<THINKING>loud plan</think>Done deal.", "Done deal.", "loud plan"},
		// Angle bracket that is not a thinking tag stays speakable.
		{"Books < movies, honestly.", "Books < movies, honestly.", ""},
	}
	for _, c := range cases {
		clean, thinking := splitThinking(c.in)
		if clean != c.clean || thinking != c.thinking {
			t.Fatalf("splitThinking(%q) = (%q, %q), want (%q, %q)",
				c.in, clean, thinking, c.clean, c.thinking)
		}
	}
}

// Content-embedded thinking must stream out as kind:thought (dropped by the
// bridge), never as speakable content — the 2026-07-06 CoT leak spoke twelve
// planning lines at a shelf visitor because they arrived as plain content.
func TestBeginStream_ThinkingSpansRoutedToThoughtKind(t *testing.T) {
	ch := newTestVoiceChannel(t)
	ch.config.Streaming = config.StreamingConfig{
		Enabled:         true,
		ThrottleSeconds: 60,
		MinGrowthChars:  1000,
	}
	if err := ch.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer ch.Stop(context.Background())

	clientConn, received, cleanup := newTestVoiceWebSocket(t)
	defer cleanup()
	ch.addConnForTest(&voiceConn{id: "conn-1", conn: clientConn, sessionID: "sess-1"})

	streamer, err := ch.BeginStream(context.Background(), "voice:sess-1")
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}

	// Mid-thought (unclosed tag): only a thought frame may go out.
	if err := streamer.Update(context.Background(), "<thinking>They seem confused."); err != nil {
		t.Fatalf("Update(thinking) error = %v", err)
	}
	thought := mustReceiveVoiceMessage(t, received)
	if thought.Type != TypeMessageCreate {
		t.Fatalf("thought type = %q, want %q", thought.Type, TypeMessageCreate)
	}
	if got := thought.Payload[PayloadKeyKind]; got != MessageKindThought {
		t.Fatalf("thought kind = %#v, want %q", got, MessageKindThought)
	}
	assertNoVoiceMessage(t, received)

	// Thought closes, reply arrives: the content frame carries ONLY the reply.
	full := "<thinking>They seem confused.</thinking>Take any book you like."
	if err := streamer.Update(context.Background(), full); err != nil {
		t.Fatalf("Update(reply) error = %v", err)
	}
	reply := mustReceiveVoiceMessage(t, received)
	if reply.Type != TypeMessageCreate {
		t.Fatalf("reply type = %q, want %q", reply.Type, TypeMessageCreate)
	}
	if got := reply.Payload[PayloadKeyContent]; got != "Take any book you like." {
		t.Fatalf("reply content = %#v, want clean text only", got)
	}
	if got := reply.Payload[PayloadKeyKind]; got != nil {
		t.Fatalf("reply kind = %#v, want unset", got)
	}
}

// The non-streaming Send fallback gets the same hygiene.
func TestSend_StripsThinking(t *testing.T) {
	ch := newTestVoiceChannel(t)
	if err := ch.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer ch.Stop(context.Background())

	clientConn, received, cleanup := newTestVoiceWebSocket(t)
	defer cleanup()
	ch.addConnForTest(&voiceConn{id: "conn-3", conn: clientConn, sessionID: "sess-3"})

	if _, err := ch.Send(context.Background(), bus.OutboundMessage{
		ChatID:  "voice:sess-3",
		Content: "<think>keep it short</think>Take any book home.",
	}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	thought := mustReceiveVoiceMessage(t, received)
	if got := thought.Payload[PayloadKeyKind]; got != MessageKindThought {
		t.Fatalf("first frame kind = %#v, want %q (thought precedes reply)", got, MessageKindThought)
	}
	reply := mustReceiveVoiceMessage(t, received)
	if got := reply.Payload[PayloadKeyContent]; got != "Take any book home." {
		t.Fatalf("reply content = %#v, want stripped text", got)
	}
	if final, _ := reply.Payload[PayloadKeyFinal].(bool); !final {
		t.Fatalf("reply final = %#v, want true", reply.Payload[PayloadKeyFinal])
	}
}

func TestBeginStream_DisabledWhenSentenceFlushOff(t *testing.T) {
	ch := newTestVoiceChannel(t)
	off := false
	ch.config.SentenceFlush = &off
	ch.config.Streaming = config.StreamingConfig{
		Enabled:         true,
		ThrottleSeconds: 60,
		MinGrowthChars:  1000,
	}
	if err := ch.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer ch.Stop(context.Background())

	clientConn, received, cleanup := newTestVoiceWebSocket(t)
	defer cleanup()
	ch.addConnForTest(&voiceConn{id: "conn-1", conn: clientConn, sessionID: "sess-1"})

	streamer, err := ch.BeginStream(context.Background(), "voice:sess-1")
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}

	if err := streamer.Update(context.Background(), "Hi"); err != nil {
		t.Fatalf("Update(first) error = %v", err)
	}
	_ = mustReceiveVoiceMessage(t, received)

	// Even with a sentence-ender, throttle should now block.
	if err := streamer.Update(context.Background(), "Hi there."); err != nil {
		t.Fatalf("Update(second) error = %v", err)
	}
	assertNoVoiceMessage(t, received)
}

func TestLastSentenceEndIndex(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", -1},
		{"abc", -1},
		{"hi.", 2},
		{"hi. there", 2},
		{"a. b? c!", 7},
		{"你好。再见！", 5},
		{"line\nnext", 4},
	}
	for _, tc := range cases {
		if got := lastSentenceEndIndex(tc.in); got != tc.want {
			t.Errorf("lastSentenceEndIndex(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func mustReceiveVoiceMessage(t *testing.T, received <-chan VoiceMessage) VoiceMessage {
	t.Helper()
	select {
	case msg := <-received:
		return msg
	case <-time.After(time.Second):
		t.Fatal("expected voice message")
	}
	return VoiceMessage{}
}

func assertNoVoiceMessage(t *testing.T, received <-chan VoiceMessage) {
	t.Helper()
	select {
	case msg := <-received:
		t.Fatalf("unexpected voice message: %+v", msg)
	case <-time.After(150 * time.Millisecond):
	}
}

// addConnForTest registers a test-built voiceConn directly into the channel's
// connection indexes, bypassing the WebSocket upgrade dance.
func (c *VoiceChannel) addConnForTest(vc *voiceConn) {
	c.connsMu.Lock()
	defer c.connsMu.Unlock()
	if c.connections == nil {
		c.connections = make(map[string]*voiceConn)
	}
	if c.sessionConnections == nil {
		c.sessionConnections = make(map[string]map[string]*voiceConn)
	}
	if _, exists := c.connections[vc.id]; exists {
		panic(fmt.Sprintf("duplicate conn id in test: %s", vc.id))
	}
	c.connections[vc.id] = vc
	bySession, ok := c.sessionConnections[vc.sessionID]
	if !ok {
		bySession = make(map[string]*voiceConn)
		c.sessionConnections[vc.sessionID] = bySession
	}
	bySession[vc.id] = vc
}

func newTestVoiceWebSocket(t *testing.T) (*websocket.Conn, <-chan VoiceMessage, func()) {
	t.Helper()

	received := make(chan VoiceMessage, 4)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer conn.Close()
		for {
			var msg VoiceMessage
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			received <- msg
		}
	}))

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		server.Close()
		t.Fatalf("Dial() error = %v", err)
	}

	cleanup := func() {
		clientConn.Close()
		server.Close()
	}
	defer resp.Body.Close()
	return clientConn, received, cleanup
}
