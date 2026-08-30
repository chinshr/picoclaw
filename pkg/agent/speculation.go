package agent

import (
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

// speculationCarryTTL bounds how long an aborted speculation's tool-call
// decision stays replayable. The bridge re-runs the transcript within
// milliseconds of the abort, so this is generous; it exists so that the same
// sentence said again much later in a session cannot replay a decision the
// model made against a materially older context.
const speculationCarryTTL = 30 * time.Second

// speculationCarryMaxEntries caps the carry map. One entry per session key is
// the norm (the bridge enforces single-flight speculation per session); the cap
// only matters for a gateway serving many sessions where some never come back
// to claim their carry.
const speculationCarryMaxEntries = 64

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
	// carry holds, per session key, the tool-call decision an aborted
	// speculation produced. See carriedDecision.
	carry map[string]*carriedDecision
}

type speculationEntry struct {
	sessionKey  string
	baseLen     int
	baseSummary string
}

// carriedDecision is the LLM response a speculative turn produced immediately
// before it was aborted for requesting a tool
// (library-claw docs/software/voice-turn-triage/01-speculation-tool-call-carry.md).
//
// Not executing tools inside a speculation is the policy and does not change —
// side effects cannot be rolled back. But the *decision* is pure: the model
// looked at this exact context and said "call these tools with these
// arguments". Discarding it means the bridge's re-run pays a second, identical
// provider round trip to re-derive it — measured at 4.5-5.9 s per tool turn on
// the library shelf, on every tool turn.
type carriedDecision struct {
	userMessage string
	response    *providers.LLMResponse
	recordedAt  time.Time
}

func newSpeculationManager() *speculationManager {
	return &speculationManager{
		pending: make(map[string]*speculationEntry),
		carry:   make(map[string]*carriedDecision),
	}
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

// recordCarry keeps the tool-call decision of a speculation that is about to
// abort, so the re-run of the SAME user message can replay it instead of paying
// an identical provider round trip. Called from the speculative-abort branch in
// pipeline_llm, after the response is in hand and before any tool has run.
//
// The response is shallow-copied (including the tool-call slice) so later
// mutation of the speculative turn's own response — reasoning suppression, hook
// rewrites — cannot reach through into the replay.
//
// iteration is the turn iteration the decision was made on, and only 1 is
// recorded. The carry is keyed by the ROOT user message, so it is a truthful key
// only when the decision was a function of that message alone. A speculation
// reaches iteration 2+ because steering was injected — the real transcript
// arriving after an early interim was speculated on — and the decision it then
// makes comes from the STEERING text while userMessage is still the stub
// interim. Field 2026-08-30: interim "Can you" was speculated, the final "Can
// you give the dog some water?" arrived as steering, read_file was decided at
// iteration 2, and the pre-guard code filed it under "Can you".
func (m *speculationManager) recordCarry(
	sessionKey, userMessage string,
	iteration int,
	resp *providers.LLMResponse,
) {
	if m == nil || resp == nil || iteration != 1 {
		return
	}
	key := strings.TrimSpace(sessionKey)
	text := strings.TrimSpace(userMessage)
	// An empty user message cannot be matched safely: every turn without a root
	// user message would look like every other one.
	if key == "" || text == "" || len(resp.ToolCalls) == 0 {
		return
	}

	snapshot := *resp
	snapshot.ToolCalls = append([]providers.ToolCall(nil), resp.ToolCalls...)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.purgeExpiredCarryLocked()
	if len(m.carry) >= speculationCarryMaxEntries {
		if _, ok := m.carry[key]; !ok {
			return
		}
	}
	m.carry[key] = &carriedDecision{
		userMessage: text,
		response:    &snapshot,
		recordedAt:  time.Now(),
	}
}

// takeCarry returns the carried decision for this session if it was recorded for
// exactly this user message and has not expired.
//
// A carry is valid for the IMMEDIATELY NEXT turn of its session and no other, so
// this consumes the entry on every lookup — hit or miss. A miss means the next
// turn was not the re-run (the bridge sent a `voice.reply.already_spoken` event,
// or the visitor said something else), and once that has happened the decision
// was made against a context the session has since moved past. Leaving it armed
// for some later turn that happens to repeat the same sentence would replay a
// stale decision; voiding it costs one ordinary LLM call.
//
// The match is deliberately narrow — exact root user message, nothing else.
// Matching a steering message instead would make the `already_spoken` re-run hit,
// and that re-run is precisely the case where the model legitimately changes its
// mind (field 2026-08-28 turn_0012: with already_spoken in context it answered
// directly instead of calling read_file). Missing that case at full cost is the
// correct trade.
func (m *speculationManager) takeCarry(sessionKey, userMessage string) *providers.LLMResponse {
	if m == nil {
		return nil
	}
	key := strings.TrimSpace(sessionKey)
	if key == "" {
		return nil
	}

	m.mu.Lock()
	entry := m.carry[key]
	delete(m.carry, key)
	m.mu.Unlock()

	if entry == nil {
		return nil
	}
	text := strings.TrimSpace(userMessage)
	if text == "" || entry.userMessage != text {
		return nil
	}
	if time.Since(entry.recordedAt) > speculationCarryTTL {
		return nil
	}
	return entry.response
}

// purgeExpiredCarryLocked drops timed-out carries. Caller holds mu.
func (m *speculationManager) purgeExpiredCarryLocked() {
	now := time.Now()
	for k, v := range m.carry {
		if now.Sub(v.recordedAt) > speculationCarryTTL {
			delete(m.carry, k)
		}
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
