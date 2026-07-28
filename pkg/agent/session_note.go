package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// sessionNoteMaxTextLen bounds a single mirrored line.
const sessionNoteMaxTextLen = 8192

// sessionNoteRequest is the wire payload for /hooks/session-note.
type sessionNoteRequest struct {
	Channel  string `json:"channel"`
	ChatID   string `json:"chat_id"`
	SenderID string `json:"sender_id,omitempty"`
	Text     string `json:"text"`
}

// AppendSessionNote records text in the conversation history of the session
// that (channel, chat_id) routes to, as an assistant message, without running
// a turn.
//
// This exists because out-of-band workers (the library check-in worker) post
// lines to the chat over the platform API directly. The user reads them as
// "the bot said this", but the agent's session history never sees them — so a
// follow-up like "2." (answering a numbered list the worker posted) reaches a
// model with no idea any list exists, and it improvises. Mirroring every
// worker-posted line here makes the session history match the conversation
// the user is actually having.
func (al *AgentLoop) AppendSessionNote(channel, chatID, senderID, text string) (string, error) {
	channel = strings.TrimSpace(channel)
	chatID = strings.TrimSpace(chatID)
	text = strings.TrimSpace(text)
	if channel == "" || chatID == "" {
		return "", fmt.Errorf("session note requires channel and chat_id")
	}
	if text == "" {
		return "", fmt.Errorf("session note requires text")
	}
	if len(text) > sessionNoteMaxTextLen {
		text = text[:sessionNoteMaxTextLen]
	}
	if senderID == "" {
		senderID = "worker"
	}

	// Resolve exactly as an inbound message on this chat would, so the note
	// lands in the same session the agent will load on the next turn.
	msg := bus.InboundMessage{
		Channel:  channel,
		ChatID:   chatID,
		SenderID: senderID,
		Content:  text,
	}
	route, agentInst, err := al.resolveMessageRoute(msg)
	if err != nil {
		return "", err
	}
	if agentInst.Sessions == nil {
		return "", fmt.Errorf("agent %q has no session store", route.AgentID)
	}
	allocation := al.allocateRouteSession(route, msg)

	agentInst.Sessions.AddMessage(allocation.SessionKey, "assistant", text)
	if err := agentInst.Sessions.Save(allocation.SessionKey); err != nil {
		logger.WarnCF("agent", "session note: save failed", map[string]any{
			"session_key": allocation.SessionKey,
			"error":       err.Error(),
		})
	}
	logger.InfoCF("agent", "Session note appended", map[string]any{
		"channel":     channel,
		"chat_id":     chatID,
		"session_key": allocation.SessionKey,
		"text_len":    len(text),
	})
	return allocation.SessionKey, nil
}

// SessionNoteHandler serves POST /hooks/session-note.
//
// Auth is a single shared token — config.json “gateway.hooks_token“ or the
// PICOCLAW_HOOKS_TOKEN env override (env.Parse applies it at config load like
// every other secret) — sent as "Authorization: Bearer <token>" or
// "X-Hooks-Token: <token>". With no token configured the endpoint refuses
// every request rather than being open. Read per-request from GetConfig so a
// hot reload picks up token changes.
func (al *AgentLoop) SessionNoteHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		token := ""
		if cfg := al.GetConfig(); cfg != nil {
			token = strings.TrimSpace(cfg.Gateway.HooksToken.String())
		}
		if token == "" {
			http.Error(
				w,
				"session-note hook disabled (set gateway.hooks_token or PICOCLAW_HOOKS_TOKEN)",
				http.StatusForbidden,
			)
			return
		}
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if got == "" {
			got = strings.TrimSpace(r.Header.Get("X-Hooks-Token"))
		}
		if got != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var req sessionNoteRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		sessionKey, err := al.AppendSessionNote(req.Channel, req.ChatID, req.SenderID, req.Text)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":          true,
			"session_key": sessionKey,
		})
	})
}
