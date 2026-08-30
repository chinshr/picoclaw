package agent

import (
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func toolCallResponse(name string) *providers.LLMResponse {
	return &providers.LLMResponse{
		Content: "Let me check.",
		ToolCalls: []providers.ToolCall{
			{ID: "call_1", Name: name, Arguments: map[string]any{"path": "/skills/vehicles/SKILL.md"}},
		},
	}
}

// The whole point of the carry: the bridge re-runs the SAME transcript and the
// decision is replayed instead of costing a second identical round trip.
func TestSpeculationCarryReplaysSameUserMessage(t *testing.T) {
	m := newSpeculationManager()
	m.recordCarry("sess-1", "How many vehicles came by today?", 1, toolCallResponse("read_file"))

	got := m.takeCarry("sess-1", "How many vehicles came by today?")
	if got == nil {
		t.Fatal("carry not replayed for an identical user message")
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "read_file" {
		t.Fatalf("replayed the wrong decision: %+v", got.ToolCalls)
	}
	if got.Content != "Let me check." {
		t.Fatalf("pre-tool content lost: %q", got.Content)
	}
}

// Whitespace differences in the transcript are not a real divergence.
func TestSpeculationCarryTrimsWhitespace(t *testing.T) {
	m := newSpeculationManager()
	m.recordCarry("sess-1", "  How many vehicles came by today?  ", 1, toolCallResponse("read_file"))

	if got := m.takeCarry("sess-1", "How many vehicles came by today?"); got == nil {
		t.Fatal("carry should match after trimming surrounding whitespace")
	}
}

// Single use: a second turn must not silently inherit the first turn's decision.
func TestSpeculationCarryIsSingleUse(t *testing.T) {
	m := newSpeculationManager()
	m.recordCarry("sess-1", "same text", 1, toolCallResponse("read_file"))

	if got := m.takeCarry("sess-1", "same text"); got == nil {
		t.Fatal("first take should hit")
	}
	if got := m.takeCarry("sess-1", "same text"); got != nil {
		t.Fatal("second take must miss — the carry is consumed")
	}
}

// The `voice.reply.already_spoken` re-run arrives with a DIFFERENT root user
// message. It must miss: with already_spoken in context the model legitimately
// changes its mind (field 2026-08-28 turn_0012 answered directly instead of
// calling read_file), so replaying the stale decision would force a tool call
// the model no longer wanted.
func TestSpeculationCarryMissesDivergentUserMessage(t *testing.T) {
	m := newSpeculationManager()
	m.recordCarry("sess-1", "I think nothing came out yet.", 1, toolCallResponse("read_file"))

	got := m.takeCarry("sess-1", "[SYSTEM EVENT] voice.reply.already_spoken {...}")
	if got != nil {
		t.Fatal("a divergent user message must not replay the carried decision")
	}
	// ...and the mismatch consumes it, so a later identical text cannot pick up
	// a decision made against a context that has since moved on.
	if got := m.takeCarry("sess-1", "I think nothing came out yet."); got != nil {
		t.Fatal("a mismatched take must invalidate the carry, not leave it armed")
	}
}

// Carries never cross sessions.
func TestSpeculationCarryIsPerSession(t *testing.T) {
	m := newSpeculationManager()
	m.recordCarry("sess-1", "same text", 1, toolCallResponse("read_file"))

	if got := m.takeCarry("sess-2", "same text"); got != nil {
		t.Fatal("another session must not see this session's carry")
	}
}

func TestSpeculationCarryExpires(t *testing.T) {
	m := newSpeculationManager()
	m.recordCarry("sess-1", "same text", 1, toolCallResponse("read_file"))

	m.mu.Lock()
	m.carry["sess-1"].recordedAt = time.Now().Add(-speculationCarryTTL - time.Second)
	m.mu.Unlock()

	if got := m.takeCarry("sess-1", "same text"); got != nil {
		t.Fatal("an expired carry must not replay")
	}
}

// A response with no tool calls is not a decision worth carrying — the turn
// that produced it committed normally.
func TestSpeculationCarryIgnoresToollessResponse(t *testing.T) {
	m := newSpeculationManager()
	m.recordCarry("sess-1", "What time is it?", 1, &providers.LLMResponse{Content: "It's about 9:30 PM."})

	if got := m.takeCarry("sess-1", "What time is it?"); got != nil {
		t.Fatal("a tool-less response must not be carried")
	}
}

// An empty root user message cannot be matched safely — every turn without one
// would look like every other one.
func TestSpeculationCarryIgnoresEmptyUserMessage(t *testing.T) {
	m := newSpeculationManager()
	m.recordCarry("sess-1", "   ", 1, toolCallResponse("read_file"))

	if got := m.takeCarry("sess-1", ""); got != nil {
		t.Fatal("an empty user message must never match a carry")
	}
}

// The snapshot must survive later mutation of the speculative turn's own
// response (reasoning suppression, AfterLLM hook rewrites).
func TestSpeculationCarrySnapshotsResponse(t *testing.T) {
	m := newSpeculationManager()
	resp := toolCallResponse("read_file")
	m.recordCarry("sess-1", "same text", 1, resp)

	resp.Content = ""
	resp.ToolCalls[0].Name = "rm_rf"
	resp.ToolCalls = nil

	got := m.takeCarry("sess-1", "same text")
	if got == nil {
		t.Fatal("carry lost")
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "read_file" {
		t.Fatalf("carry was mutated through the original response: %+v", got.ToolCalls)
	}
	if got.Content != "Let me check." {
		t.Fatalf("carry content was mutated through the original: %q", got.Content)
	}
}

// A nil manager is the no-speculation deployment; it must be inert, not panic.
func TestSpeculationCarryNilManagerIsInert(t *testing.T) {
	var m *speculationManager
	m.recordCarry("sess-1", "same text", 1, toolCallResponse("read_file"))
	if got := m.takeCarry("sess-1", "same text"); got != nil {
		t.Fatal("nil manager must never produce a carry")
	}
}

func TestSpeculationCarryEvictsExpiredUnderPressure(t *testing.T) {
	m := newSpeculationManager()
	stale := time.Now().Add(-speculationCarryTTL - time.Second)
	for i := 0; i < speculationCarryMaxEntries; i++ {
		key := "stale-" + time.Duration(i).String()
		m.recordCarry(key, "text", 1, toolCallResponse("read_file"))
		m.mu.Lock()
		m.carry[key].recordedAt = stale
		m.mu.Unlock()
	}

	m.recordCarry("fresh", "text", 1, toolCallResponse("read_file"))
	if got := m.takeCarry("fresh", "text"); got == nil {
		t.Fatal("a fresh carry must not be refused because of expired entries")
	}
}

// A speculation reaches iteration 2+ only because steering was injected — the
// real transcript arriving after an early interim was speculated on. The
// decision it makes there is a function of the STEERING text, while the root
// user message is still the stub interim, so recording it would file the
// decision under a message that did not produce it.
//
// Field 2026-08-30: interim "Can you" was speculated; the final "Can you give
// the dog some water?" arrived as steering; read_file was decided at iteration 2
// and filed under "Can you", where it could never match and should never match.
func TestSpeculationCarryOnlyRecordsIterationOne(t *testing.T) {
	m := newSpeculationManager()

	m.recordCarry("sess-1", "Can you", 2, toolCallResponse("read_file"))
	if got := m.takeCarry("sess-1", "Can you"); got != nil {
		t.Fatal("a decision made after steering must not be carried under the stub interim")
	}

	m.recordCarry("sess-1", "Can you", 1, toolCallResponse("read_file"))
	if got := m.takeCarry("sess-1", "Can you"); got == nil {
		t.Fatal("an iteration-1 decision must still be carried")
	}
}
