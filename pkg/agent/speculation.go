package agent

import (
	"sync"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/session"
)

// speculationManager tracks in-flight speculative turns (Pico Protocol
// speculative turns, docs/design/speculative-turns.md). A speculative turn runs
// the normal pipeline against the real session but its history write is
// reversible: begin() snapshots the pre-turn history length + summary, abort()
// truncates back to it, commit() just drops the entry (the write stands).
//
// Keyed by speculation_id (client-chosen, unique per session). Single-flight
// per session is enforced by the bridge; the map tolerates multiple ids.
type speculationManager struct {
	mu      sync.Mutex
	pending map[string]*speculationEntry
}

type speculationEntry struct {
	sessionKey  string
	baseLen     int
	baseSummary string
}

func newSpeculationManager() *speculationManager {
	return &speculationManager{pending: make(map[string]*speculationEntry)}
}

// begin records the restore point for a speculative turn. Called from turn
// setup when the turn is tagged speculative. Re-begin with the same id replaces
// the entry (prefix grew → new speculation).
func (m *speculationManager) begin(specID, sessionKey string, baseLen int, baseSummary string) {
	if m == nil || specID == "" {
		return
	}
	m.mu.Lock()
	m.pending[specID] = &speculationEntry{sessionKey: sessionKey, baseLen: baseLen, baseSummary: baseSummary}
	m.mu.Unlock()
}

// commit keeps a speculative turn: the history write already happened and is
// correct (the bridge only commits on a transcript match). Just drop the entry.
func (m *speculationManager) commit(specID string) {
	if m == nil || specID == "" {
		return
	}
	m.mu.Lock()
	delete(m.pending, specID)
	m.mu.Unlock()
}

// abort reverts a speculative turn's history write: truncate the session back to
// the snapshotted length and restore the summary. Mirrors steering.go's
// SetHistory(history[:initialHistoryLength]) rollback. Idempotent / safe if the
// entry is unknown (already committed or never registered).
func (m *speculationManager) abort(store session.SessionStore, specID string) {
	if m == nil || specID == "" || store == nil {
		return
	}
	m.mu.Lock()
	entry := m.pending[specID]
	delete(m.pending, specID)
	m.mu.Unlock()
	if entry == nil {
		return
	}
	h := store.GetHistory(entry.sessionKey)
	if len(h) > entry.baseLen {
		store.SetHistory(entry.sessionKey, h[:entry.baseLen])
	}
	store.SetSummary(entry.sessionKey, entry.baseSummary)
	if err := store.Save(entry.sessionKey); err != nil {
		logger.WarnCF("agent", "speculation abort save failed", map[string]any{
			"speculation_id": specID, "session_key": entry.sessionKey, "error": err.Error(),
		})
	}
}

// handleSpeculationControl processes a commit/abort control message intercepted
// from the inbound bus (see agent loop). It never runs a turn.
func (al *AgentLoop) handleSpeculationControl(msg bus.InboundMessage, control string) {
	specID := msg.Context.Raw[bus.RawKeySpeculationID]
	if specID == "" {
		return
	}
	switch control {
	case bus.ControlCommit:
		al.speculation.commit(specID)
	case bus.ControlAbort:
		// Resolve the agent that owns this session to reach its session store.
		_, agentID, ok := al.resolveSteeringTarget(msg)
		if !ok {
			al.speculation.commit(specID) // can't resolve store; drop bookkeeping
			return
		}
		agent, ok := al.registry.GetAgent(agentID)
		if !ok || agent == nil {
			al.speculation.commit(specID)
			return
		}
		al.speculation.abort(agent.Sessions, specID)
	default:
		al.speculation.commit(specID)
	}
}
