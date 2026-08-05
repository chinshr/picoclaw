package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// textualToolCallLineRe matches one flattened tool call, the exact shape the
// seahorse store uses when it renders a historical assistant tool call into
// plain text: `[tool_use: exec, args: {"action":"run",...}]`.
var textualToolCallLineRe = regexp.MustCompile(
	`^\[tool_use:\s*([A-Za-z0-9_.\-]+)\s*,\s*args:\s*(\{.*\})\s*\]$`,
)

// textualToolCallMax bounds how many flattened calls one response may carry.
const textualToolCallMax = 8

// parseTextualToolCalls recovers tool calls from a response whose CONTENT is
// nothing but flattened "[tool_use: name, args: {...}]" blocks.
//
// Session history shows the model its own past tool calls in that flattened
// text form, and some models occasionally imitate it — emitting the tool call
// as text instead of a native call (field 2026-07-27 22:59: a check-in photo
// got its ingest command posted to Telegram as the reply, and nothing ran).
// Such a response is not a direct answer; it is a tool call in the wrong
// encoding.
//
// Deliberately strict: every non-empty line must be a well-formed block with
// JSON-object args, else nil — prose that merely mentions the syntax is left
// untouched and delivered as text.
func parseTextualToolCalls(content string) []providers.ToolCall {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" || !strings.HasPrefix(trimmed, "[tool_use:") {
		return nil
	}

	var calls []providers.ToolCall
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := textualToolCallLineRe.FindStringSubmatch(line)
		if m == nil {
			return nil
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(m[2]), &args); err != nil {
			return nil
		}
		if len(calls) >= textualToolCallMax {
			return nil
		}
		calls = append(calls, providers.ToolCall{
			ID: fmt.Sprintf("textual_%d", len(calls)),
			Function: &providers.FunctionCall{
				Name:      m[1],
				Arguments: m[2],
			},
		})
	}
	return calls
}
