package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
)

// The note must land in the exact session an inbound message on that chat
// would use — otherwise the mirror writes history nobody ever reads.
func TestAppendSessionNote_LandsInTheRoutedSession(t *testing.T) {
	al, _, _, _, cleanup := newTestAgentLoop(t)
	defer cleanup()

	text := "Did you mean:\n1. Beebo Brinker by Ann Bannon\n2. Twilight by Kit Gardner"
	sessionKey, err := al.AppendSessionNote("telegram", "8755018511", "library-worker", text)
	if err != nil {
		t.Fatalf("AppendSessionNote failed: %v", err)
	}

	msg := bus.InboundMessage{Channel: "telegram", ChatID: "8755018511", SenderID: "user"}
	route, agentInst, err := al.resolveMessageRoute(msg)
	if err != nil {
		t.Fatalf("resolveMessageRoute failed: %v", err)
	}
	allocation := al.allocateRouteSession(route, msg)
	if allocation.SessionKey != sessionKey {
		t.Fatalf("note session %q != inbound session %q", sessionKey, allocation.SessionKey)
	}

	history := agentInst.Sessions.GetHistory(allocation.SessionKey)
	if len(history) != 1 {
		t.Fatalf("expected 1 history message, got %d", len(history))
	}
	if history[0].Role != "assistant" {
		t.Errorf("role = %q, want assistant", history[0].Role)
	}
	if !strings.Contains(history[0].Content, "Beebo Brinker") {
		t.Errorf("content missing note text: %q", history[0].Content)
	}
}

func TestAppendSessionNote_RequiresChannelChatAndText(t *testing.T) {
	al, _, _, _, cleanup := newTestAgentLoop(t)
	defer cleanup()

	if _, err := al.AppendSessionNote("", "1", "w", "x"); err == nil {
		t.Error("expected error for missing channel")
	}
	if _, err := al.AppendSessionNote("telegram", "", "w", "x"); err == nil {
		t.Error("expected error for missing chat_id")
	}
	if _, err := al.AppendSessionNote("telegram", "1", "w", "  "); err == nil {
		t.Error("expected error for empty text")
	}
}

func postSessionNote(t *testing.T, h http.Handler, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/hooks/session-note", bytes.NewReader(raw))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSessionNoteHandler_AuthAndAppend(t *testing.T) {
	al, cfg, _, _, cleanup := newTestAgentLoop(t)
	defer cleanup()
	handler := al.SessionNoteHandler()

	body := sessionNoteRequest{Channel: "telegram", ChatID: "42", Text: "worker line"}

	// No token configured: refuse everything, even a matching guess.
	if rec := postSessionNote(t, handler, "anything", body); rec.Code != http.StatusForbidden {
		t.Fatalf("unconfigured token: code = %d, want 403", rec.Code)
	}

	cfg.Gateway.HooksToken.Set("secret")
	if rec := postSessionNote(t, handler, "wrong", body); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: code = %d, want 401", rec.Code)
	}

	rec := postSessionNote(t, handler, "secret", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	sessionKey, _ := resp["session_key"].(string)
	if sessionKey == "" {
		t.Fatal("response missing session_key")
	}

	agentInst := al.GetRegistry().GetDefaultAgent()
	history := agentInst.Sessions.GetHistory(sessionKey)
	if len(history) != 1 || history[0].Content != "worker line" {
		t.Fatalf("unexpected history: %+v", history)
	}
}

func TestSessionNoteHandler_RejectsNonPost(t *testing.T) {
	al, _, _, _, cleanup := newTestAgentLoop(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/hooks/session-note", nil)
	rec := httptest.NewRecorder()
	al.SessionNoteHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d, want 405", rec.Code)
	}
}
