package agent

import (
	"context"
	"os"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// carryProbeTool is the tool the speculation decides to call. It records how
// often it actually ran, so the test can assert the speculation never executed
// it while the re-run did.
type carryProbeTool struct{ runs int }

func (t *carryProbeTool) Name() string        { return "carry_probe" }
func (t *carryProbeTool) Description() string { return "Probe tool for speculation carry tests" }
func (t *carryProbeTool) Parameters() map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": true,
	}
}
func (t *carryProbeTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	t.runs++
	return tools.SilentResult("one vehicle")
}

// carryProbeProvider answers with a tool call until the tool result shows up in
// the conversation, then answers directly. Every call is counted and its
// messages kept, which is what makes the saved round trip observable.
type carryProbeProvider struct {
	calls        int
	toolCalls    int
	finalAnswers int
}

func (p *carryProbeProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	p.calls++
	for _, m := range messages {
		if m.Role == "tool" {
			p.finalAnswers++
			return &providers.LLMResponse{Content: "Just one vehicle so far today."}, nil
		}
	}
	p.toolCalls++
	return &providers.LLMResponse{
		Content: "Let me check.",
		ToolCalls: []providers.ToolCall{{
			ID:        "call_carry",
			Type:      "function",
			Name:      "carry_probe",
			Arguments: map[string]any{},
		}},
	}, nil
}

func (p *carryProbeProvider) GetDefaultModel() string { return "carry-probe-model" }

func newCarryProbeLoop(t *testing.T) (*AgentLoop, *carryProbeProvider, *carryProbeTool) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "carry-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         tmpDir,
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}
	provider := &carryProbeProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	probe := &carryProbeTool{}
	al.RegisterTool(probe)
	return al, provider, probe
}

func carryTurn(t *testing.T, al *AgentLoop, text string, speculative bool) string {
	t.Helper()
	msg := testInboundMessage(bus.InboundMessage{
		Channel:  "voice_bridge",
		SenderID: "voice-bridge-user",
		ChatID:   "carry-session",
		Content:  text,
	})
	if speculative {
		if msg.Context.Raw == nil {
			msg.Context.Raw = map[string]string{}
		}
		msg.Context.Raw[bus.RawKeySpeculative] = "1"
		msg.Context.Raw[bus.RawKeySpeculationID] = "spec_carry_1"
	}
	resp, err := al.processMessage(context.Background(), msg)
	if err != nil {
		t.Fatalf("processMessage(%q, speculative=%v) error = %v", text, speculative, err)
	}
	return resp
}

// The headline behaviour: a speculation that aborts on a tool call must not make
// the re-run of the SAME transcript pay for that decision again.
//
// Without the carry this costs three provider calls — the speculation's
// discarded decision, the re-run re-deriving the identical decision, and the
// post-tool answer. With it, two.
func TestSpeculationCarrySavesTheDuplicateRoundTrip(t *testing.T) {
	al, provider, probe := newCarryProbeLoop(t)
	const text = "How many vehicles came by today?"

	if got := carryTurn(t, al, text, true); got != "" {
		t.Fatalf("speculative turn should end empty on a tool call, got %q", got)
	}
	if provider.calls != 1 {
		t.Fatalf("speculation made %d provider calls, want 1", provider.calls)
	}
	if probe.runs != 0 {
		t.Fatalf("speculation executed the tool %d times — it must never run one", probe.runs)
	}

	answer := carryTurn(t, al, text, false)
	if answer != "Just one vehicle so far today." {
		t.Fatalf("re-run answer = %q", answer)
	}
	if probe.runs != 1 {
		t.Fatalf("tool ran %d times in the re-run, want 1", probe.runs)
	}
	if provider.calls != 2 {
		t.Fatalf("total provider calls = %d, want 2 (the duplicate decision call was not saved)", provider.calls)
	}
	if provider.toolCalls != 1 {
		t.Fatalf("the tool DECISION was derived %d times, want 1", provider.toolCalls)
	}
	if provider.finalAnswers != 1 {
		t.Fatalf("final answers = %d, want 1", provider.finalAnswers)
	}
}

// A re-run with different text is a different question: the carry must be
// ignored AND voided, so the turn pays for its own decision.
func TestSpeculationCarryNotReusedForDifferentText(t *testing.T) {
	al, provider, probe := newCarryProbeLoop(t)

	carryTurn(t, al, "How many vehicles came by today?", true)
	if provider.calls != 1 {
		t.Fatalf("speculation made %d provider calls, want 1", provider.calls)
	}

	carryTurn(t, al, "Actually, never mind — what time is it?", false)
	if provider.toolCalls != 2 {
		t.Fatalf("divergent turn derived %d decisions, want 2 (its own, not the carried one)", provider.toolCalls)
	}
	if probe.runs != 1 {
		t.Fatalf("tool ran %d times, want 1 (the divergent turn's own call)", probe.runs)
	}
}

// A plain turn that was never preceded by an aborted speculation must behave
// exactly as before: derive its own decision, run the tool, answer.
func TestSpeculationCarryAbsentLeavesNormalTurnUnchanged(t *testing.T) {
	al, provider, probe := newCarryProbeLoop(t)

	answer := carryTurn(t, al, "How many vehicles came by today?", false)
	if answer != "Just one vehicle so far today." {
		t.Fatalf("answer = %q", answer)
	}
	if provider.calls != 2 || probe.runs != 1 {
		t.Fatalf("provider calls = %d (want 2), tool runs = %d (want 1)", provider.calls, probe.runs)
	}
}
