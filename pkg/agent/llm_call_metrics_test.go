package agent

import (
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// The system prompt is the biggest block there is, and on a cache-aware adapter
// it exists TWICE in the same message: as blocks in SystemParts, and as their
// concatenation in Content for adapters that do not read blocks. Exactly one
// goes on the wire. The first version of this counter summed both and reported
// 77,642 chars for a prompt of about 39,000 — a factor of two on the only
// number the function exists to produce, in the direction that would have sent
// us hunting a prompt-size problem twice as large as the real one.
func TestPromptCharsDoesNotDoubleCountSystemParts(t *testing.T) {
	msgs := []providers.Message{
		{
			Role:    "system",
			Content: "0123456789abcde", // 15: the concatenation
			SystemParts: []providers.ContentBlock{
				{Type: "text", Text: "0123456789"}, // 10
				{Type: "text", Text: "abcde"},      // 5
			},
		},
		{Role: "user", Content: "hello"}, // 5
	}
	got, _ := requestCharCounts(msgs, nil)
	if got != 20 {
		t.Fatalf("promptChars = %d, want 20 (15 system + 5 user, counted once)", got)
	}
}

// When only the blocks are populated they must still be counted: an adapter
// that fills SystemParts and leaves Content empty would otherwise report the
// largest message in the request as zero.
func TestPromptCharsCountsSystemPartsWhenContentIsEmpty(t *testing.T) {
	msgs := []providers.Message{
		{Role: "system", SystemParts: []providers.ContentBlock{
			{Type: "text", Text: "0123456789"}, // 10
			{Type: "text", Text: "abcde"},      // 5
		}},
		{Role: "user", Content: "hello"}, // 5
	}
	got, _ := requestCharCounts(msgs, nil)
	if got != 20 {
		t.Fatalf("promptChars = %d, want 20", got)
	}
}

// "The prompt is 39k characters" is a fact. "12k of them are the workspace
// bootstrap files" is a decision about what to cut, which is the only reason
// the fact is interesting. Largest slot first, and stable between turns so two
// log lines can be compared by eye.
func TestSystemSlotCharsRanksLargestFirst(t *testing.T) {
	msgs := []providers.Message{
		{Role: "system", SystemParts: []providers.ContentBlock{
			{Type: "text", Text: "abc", PromptSlot: "skill_catalog"},
			{Type: "text", Text: "0123456789", PromptSlot: "workspace"},
			{Type: "text", Text: "xy", PromptSlot: "skill_catalog"},
		}},
	}
	got := systemSlotChars(msgs)
	if got != "workspace=10 skill_catalog=5" {
		t.Fatalf("prompt_slots = %q, want \"workspace=10 skill_catalog=5\"", got)
	}
}

// A block with no slot is still part of the prompt; dropping it would make the
// breakdown quietly fail to add up to prompt_chars.
func TestSystemSlotCharsLabelsUnattributedBlocks(t *testing.T) {
	msgs := []providers.Message{
		{Role: "system", SystemParts: []providers.ContentBlock{{Type: "text", Text: "abcd"}}},
	}
	if got := systemSlotChars(msgs); got != "unattributed=4" {
		t.Fatalf("prompt_slots = %q", got)
	}
}

// No blocks means no breakdown, and the field is omitted rather than logged
// empty.
func TestSystemSlotCharsEmptyWithoutBlocks(t *testing.T) {
	msgs := []providers.Message{{Role: "system", Content: "plain"}}
	if got := systemSlotChars(msgs); got != "" {
		t.Fatalf("prompt_slots = %q, want empty", got)
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
		p, c, total, cached := usageCounts(resp)
		if p != 0 || c != 0 || total != 0 || cached != 0 {
			t.Fatalf("%s: got %d/%d/%d/%d, want zeros", name, p, c, total, cached)
		}
	}
}

func TestUsageCountsReadsReportedUsage(t *testing.T) {
	resp := &providers.LLMResponse{Usage: &providers.UsageInfo{
		PromptTokens: 12000, CompletionTokens: 40, TotalTokens: 12040,
	}}
	p, c, total, cached := usageCounts(resp)
	if p != 12000 || c != 40 || total != 12040 {
		t.Fatalf("got %d/%d/%d", p, c, total)
	}
	if cached != 0 {
		t.Fatalf("cached = %d, want 0 when the provider reported none", cached)
	}
}

// Finding 15's decisive number, in both shapes providers use for it.
//
// Kimi prefix caching is fully automatic: nothing in the request switches it on
// or off, and its only requirement is a byte-stable prefix. So a cache hit and
// a cold prefill are indistinguishable from our side — cached_tokens in the
// response is the whole of the evidence, and dropping it on the floor (which is
// what happened before 2026-09-02) leaves the question unanswerable.
func TestUsageCountsReadsCachedTokensFromEitherShape(t *testing.T) {
	nested := &providers.LLMResponse{Usage: &providers.UsageInfo{
		PromptTokens:        12000,
		PromptTokensDetails: &providers.PromptTokensDetails{CachedTokens: 11776},
	}}
	if _, _, _, cached := usageCounts(nested); cached != 11776 {
		t.Fatalf("nested cached = %d, want 11776", cached)
	}

	flat := &providers.LLMResponse{Usage: &providers.UsageInfo{
		PromptTokens: 12000, CachedTokens: 11776,
	}}
	if _, _, _, cached := usageCounts(flat); cached != 11776 {
		t.Fatalf("flat cached = %d, want 11776", cached)
	}
}

// A provider that omits the field reports zero, and so does a genuine cold
// prefill. The two are not distinguishable here, which is why the payload
// comment says to read cached_tokens alongside prompt_tokens rather than alone.
func TestZeroCachedTokensIsNotEvidenceOfACacheMiss(t *testing.T) {
	resp := &providers.LLMResponse{Usage: &providers.UsageInfo{PromptTokens: 12000}}
	if _, _, _, cached := usageCounts(resp); cached != 0 {
		t.Fatalf("cached = %d, want 0", cached)
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
