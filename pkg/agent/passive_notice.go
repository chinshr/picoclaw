package agent

import (
	"strings"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// handlePassiveNotice records a no-reply message in the session history and
// runs nothing else.
//
// A passive notice is a statement of fact the next real answer needs to be
// consistent with — "I gave up waiting and apologised to the visitor", "that
// reply was composed but never spoken". It is not a request. Routed through
// the ordinary path it became one of two equally wrong things: a root user
// message that started its own turn (a full provider call spent on the bridge
// talking to itself, with the visitor's real question demoted to steering
// inside it), or steering that continued a turn which had already answered.
//
// Both cost a model call per notice, and for voice.turn.timeout that call sits
// inside the loop that produced the notice. Appending straight to history costs
// nothing and keeps the history honest, which was the only reason the notice
// existed.
//
// Returns false when the session cannot be resolved, so the caller falls back
// to the normal path rather than silently dropping the line.
func (al *AgentLoop) handlePassiveNotice(msg bus.InboundMessage) bool {
	if strings.TrimSpace(msg.Content) == "" {
		return true // nothing to record, and still not a turn
	}

	sessionKey, agentID, ok := al.resolveSteeringTarget(msg)
	if !ok {
		return false
	}
	agent, ok := al.registry.GetAgent(agentID)
	if !ok || agent == nil || agent.Sessions == nil {
		return false
	}

	agent.Sessions.AddMessage(sessionKey, "user", msg.Content)

	logger.InfoCF("agent", "Passive notice recorded (no turn)", map[string]any{
		"channel":     msg.Channel,
		"chat_id":     msg.ChatID,
		"session_key": sessionKey,
		"content_len": len(msg.Content),
	})
	return true
}
