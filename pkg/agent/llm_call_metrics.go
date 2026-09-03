package agent

// Finding 15 (library-claw docs/software/voice-turn-triage/15-provider-call-time.md).
//
// Every finding in that folder is about not paying for the model call more
// times than necessary. None of them touch the call itself, and after the
// 09-01 deploy the call itself is the dominant term: single calls of 14-17 s,
// turns of 84-97 s. `duration_ms` is the only number we have about them, and
// it cannot distinguish the two things it could be:
//
//	huge prompt      -> the provider spends its time on prefill, and the first
//	                    token is late. Fix: send less. Findings 02, 03, 11.
//	long generation  -> the first token is prompt and the tokens keep coming.
//	                    Fix: ask for less, or a different model. Nothing in the
//	                    folder addresses it.
//
// So: measure the size going out, and split the time coming back.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// requestCharCounts measures the assembled request.
//
// Characters, not tokens: the tokenizer belongs to the provider, and every
// question this instrumentation exists to answer ("is this call slow because
// the prompt is enormous?") is answered by an order of magnitude. Counted
// before the call, so a request that never returns is still measurable — which
// is the whole point for a wedged turn.
func requestCharCounts(
	messages []providers.Message,
	tools []providers.ToolDefinition,
) (promptChars, toolsChars int) {
	for i := range messages {
		// Content and SystemParts are the SAME payload in two shapes, not two
		// payloads: context.go builds one system message carrying the blocks in
		// SystemParts for cache-aware adapters AND their concatenation in
		// Content as the fallback. Exactly one of them goes on the wire. Summing
		// both double-counts the system prompt — which is the largest block
		// there is, so the error is roughly a factor of two on the only number
		// this function exists to produce. Take the larger instead.
		chars := len(messages[i].Content)
		if parts := 0; len(messages[i].SystemParts) > 0 {
			for _, part := range messages[i].SystemParts {
				parts += len(part.Text)
			}
			if parts > chars {
				chars = parts
			}
		}
		promptChars += chars
		promptChars += len(messages[i].ReasoningContent)
		for _, tc := range messages[i].ToolCalls {
			if tc.Function == nil {
				continue
			}
			promptChars += len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	for _, t := range tools {
		toolsChars += len(t.Function.Name) + len(t.Function.Description)
	}
	return promptChars, toolsChars
}

// systemBlockChars measures the system message as it goes on the wire: one
// entry per content block, keyed by the block's slot.
//
// It reports BLOCKS, not composition, and the distinction matters. On the
// cached path — which is every ordinary turn — context.go deliberately collapses
// the whole static prompt into a SINGLE block so there is one `cache_control`
// marker to anchor the provider's prefix cache. That block is labelled
// `identity` because that is the slot its placement rule allows, and it
// contains the workspace files, the skill catalog and memory too.
//
// So `identity=39377` here does not mean 39 KB of identity. It means one
// 39 KB block, and the composition inside it is not visible from this side by
// construction.
//
// For the composition, read the `System prompt built ... slots=` line that
// ContextBuilder emits whenever it (re)builds the cached prompt — once per
// process, and again whenever a workspace file changes. It is a static property
// of the workspace, so per-request was always the wrong place for it.
//
// Returned as a sorted "slot=chars" string rather than a map: it goes into one
// log field that a person reads, and map iteration order would reshuffle it
// between turns and make two lines impossible to compare by eye.
func systemBlockChars(messages []providers.Message) string {
	totals := map[string]int{}
	for i := range messages {
		for _, part := range messages[i].SystemParts {
			slot := part.PromptSlot
			if slot == "" {
				slot = "unattributed"
			}
			totals[slot] += len(part.Text)
		}
	}
	if len(totals) == 0 {
		return ""
	}
	slots := make([]string, 0, len(totals))
	for slot := range totals {
		slots = append(slots, slot)
	}
	// Largest first: the answer to "what is this prompt made of" is the top line.
	sort.Slice(slots, func(a, b int) bool {
		if totals[slots[a]] != totals[slots[b]] {
			return totals[slots[a]] > totals[slots[b]]
		}
		return slots[a] < slots[b]
	})
	var b strings.Builder
	for i, slot := range slots {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%d", slot, totals[slot])
	}
	return b.String()
}

// usageCounts reads provider-reported usage, tolerating its absence.
//
// Not every provider returns usage, and a streaming response may return it only
// on the final frame. Zero here means "not reported", never "zero tokens" —
// pair it with prompt_chars on the request event, which is always present.
//
// `cached` is the finding-15 number. Moonshot/Kimi prefix caching is fully
// automatic and invisible from the request side, so this is the only way to
// tell a cache hit from a cold prefill — which is the difference between "our
// 39 KB prompt is nearly free" and "we pay ~10k tokens of prefill on every
// call", and those prescribe opposite work.
func usageCounts(resp *providers.LLMResponse) (prompt, completion, total, cached int) {
	if resp == nil || resp.Usage == nil {
		return 0, 0, 0, 0
	}
	return resp.Usage.PromptTokens,
		resp.Usage.CompletionTokens,
		resp.Usage.TotalTokens,
		resp.Usage.Cached()
}
