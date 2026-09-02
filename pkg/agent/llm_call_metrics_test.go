package agent

import (
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// The number this instrumentation exists to produce is "how big was the thing
// we sent", and the biggest single contributor — the system prompt, carrying
// the skills catalog — moves into SystemParts on a cache-aware adapter. A
// counter that reads Content only reports a large prompt as a small one, which
// is worse than not measuring it: it actively exonerates the real cause.
func TestPromptCharsCountsSystemParts(t *testing.T) {
	msgs := []providers.Message{
		{Role: "system", SystemParts: []providers.ContentBlock{
			{Type: "text", Text: "0123456789"}, // 10
			{Type: "text", Text: "abcde"},      // 5
		}},
		{Role: "user", Content: "hello"}, // 5
	}
	got, _ := requestCharCounts(msgs, nil)
	if got != 20 {
		t.Fatalf("promptChars = %d, want 20 (10+5 system parts + 5 content)", got)
	}
}

// Tool-call arguments are part of what gets re-sent on every subsequent
// iteration of a tool turn, and a tool turn is where the long calls live.
func TestPromptCharsCountsToolCallArguments(t *testing.T) {
	msgs := []providers.Message{
		{Role: "assistant", ToolCalls: []providers.ToolCall{
			{Function: &providers.FunctionCall{Name: "bash", Arguments: `{"cmd":"ls"}`}}, // 4 + 12
		}},
	}
	got, _ := requestCharCounts(msgs, nil)
	if got != 16 {
		t.Fatalf("promptChars = %d, want 16", got)
	}
}

// A tool call with no Function is malformed, not a reason to take the whole
// gateway down on a nil dereference in a metrics helper.
func TestPromptCharsSurvivesNilFunction(t *testing.T) {
	msgs := []providers.Message{
		{Role: "assistant", Content: "hi", ToolCalls: []providers.ToolCall{{ID: "x"}}},
	}
	got, _ := requestCharCounts(msgs, nil)
	if got != 2 {
		t.Fatalf("promptChars = %d, want 2", got)
	}
}

// Tool definitions are counted separately: they are constant across a session,
// so folding them into promptChars would hide the part that actually grows.
func TestToolCharsAreSeparate(t *testing.T) {
	tools := []providers.ToolDefinition{
		{Function: providers.ToolFunctionDefinition{Name: "bash", Description: "runs"}},
	}
	prompt, toolsChars := requestCharCounts(nil, tools)
	if prompt != 0 {
		t.Fatalf("promptChars = %d, want 0", prompt)
	}
	if toolsChars != 8 {
		t.Fatalf("toolsChars = %d, want 8", toolsChars)
	}
}

// Zero must mean "the provider did not report usage", and must never be a
// crash. prompt_chars on the request event is the fallback for this case.
func TestUsageCountsToleratesMissingUsage(t *testing.T) {
	for name, resp := range map[string]*providers.LLMResponse{
		"nil response": nil,
		"nil usage":    {Content: "hi"},
	} {
		p, c, total := usageCounts(resp)
		if p != 0 || c != 0 || total != 0 {
			t.Fatalf("%s: got %d/%d/%d, want zeros", name, p, c, total)
		}
	}
}

func TestUsageCountsReadsReportedUsage(t *testing.T) {
	resp := &providers.LLMResponse{Usage: &providers.UsageInfo{
		PromptTokens: 12000, CompletionTokens: 40, TotalTokens: 12040,
	}}
	p, c, total := usageCounts(resp)
	if p != 12000 || c != 40 || total != 12040 {
		t.Fatalf("got %d/%d/%d", p, c, total)
	}
}

// A speculation carry-forward replay (finding 01) makes no provider call. If
// it were counted, the turn's provider_ms would include time nobody spent —
// and finding 01's entire claim is that this call was NOT made. The instrument
// would then hide the improvement it exists to measure.
func TestProviderTotalsExcludeNothingItWasGiven(t *testing.T) {
	ts := &turnState{}
	ts.recordProviderCall(4*time.Second, 3500*time.Millisecond, 12000)
	ts.recordProviderCall(6*time.Second, 5*time.Second, 48000)

	calls, total, ttft, maxPrompt := ts.providerTotals()
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if total != 10*time.Second {
		t.Fatalf("total = %s, want 10s", total)
	}
	if ttft != 8500*time.Millisecond {
		t.Fatalf("ttft = %s, want 8.5s", ttft)
	}
	if maxPrompt != 48000 {
		t.Fatalf("maxPromptChars = %d, want 48000 (the largest, not the last)", maxPrompt)
	}
}

// A non-streaming call contributes duration but no TTFT. Summing a zero is
// correct — the alternative is inventing one — but it makes providerTTFT a
// lower bound, which is why the payload comment says to read it against
// ProviderCalls rather than as a mean.
func TestProviderTTFTIsALowerBound(t *testing.T) {
	ts := &turnState{}
	ts.recordProviderCall(5*time.Second, 0, 100) // did not stream
	ts.recordProviderCall(5*time.Second, 4*time.Second, 100)

	_, total, ttft, _ := ts.providerTotals()
	if total != 10*time.Second || ttft != 4*time.Second {
		t.Fatalf("total=%s ttft=%s, want 10s/4s", total, ttft)
	}
}

// A turn that made no call at all must report zeros, not a nil-map panic in
// the turn-end path.
func TestProviderTotalsZeroValue(t *testing.T) {
	ts := &turnState{}
	calls, total, ttft, maxPrompt := ts.providerTotals()
	if calls != 0 || total != 0 || ttft != 0 || maxPrompt != 0 {
		t.Fatalf("got %d/%s/%s/%d, want zeros", calls, total, ttft, maxPrompt)
	}
}
