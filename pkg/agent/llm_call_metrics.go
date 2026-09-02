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
		promptChars += len(messages[i].Content)
		// A cache-aware adapter carries the system prompt in SystemParts rather
		// than Content. Missing those would under-report the one message most
		// worth measuring — the skills catalog lives there.
		for _, part := range messages[i].SystemParts {
			promptChars += len(part.Text)
		}
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

// usageCounts reads provider-reported usage, tolerating its absence.
//
// Not every provider returns usage, and a streaming response may return it only
// on the final frame. Zero here means "not reported", never "zero tokens" —
// pair it with prompt_chars on the request event, which is always present.
func usageCounts(resp *providers.LLMResponse) (prompt, completion, total int) {
	if resp == nil || resp.Usage == nil {
		return 0, 0, 0
	}
	return resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens
}
