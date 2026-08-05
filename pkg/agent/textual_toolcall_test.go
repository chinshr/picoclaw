package agent

import (
	"strings"
	"testing"
)

// Verbatim from the 2026-07-27 22:59 incident (event never created): the model
// answered a check-in photo with its tool call rendered as text.
const incidentContent = `[tool_use: exec, args: {"action":"run","command": "library skills inventory exec item-checkin -- ingest --file \"/tmp/picoclaw_media/13fc6563_file_80.jpg\" --channel-id \"13fc6563\" --submitted-by \"Juergen\" --location library"}]`

func TestParseTextualToolCalls_RecoversTheIncidentResponse(t *testing.T) {
	calls := parseTextualToolCalls(incidentContent)

	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "exec" {
		t.Errorf("name = %q, want exec", calls[0].Function.Name)
	}
	if calls[0].ID == "" {
		t.Error("recovered call must carry an ID")
	}
	// Arguments must be the verbatim JSON so NormalizeToolCall can parse them.
	if want := `"library skills inventory exec item-checkin`; !strings.Contains(calls[0].Function.Arguments, want) {
		t.Errorf("arguments lost the command: %q", calls[0].Function.Arguments)
	}
}

func TestParseTextualToolCalls_MultipleBlocks(t *testing.T) {
	content := "[tool_use: exec, args: {\"a\":1}]\n[tool_use: read_file, args: {\"path\":\"x\"}]"

	calls := parseTextualToolCalls(content)

	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Function.Name != "exec" || calls[1].Function.Name != "read_file" {
		t.Errorf("names = %q, %q", calls[0].Function.Name, calls[1].Function.Name)
	}
	if calls[0].ID == calls[1].ID {
		t.Error("IDs must be unique within a response")
	}
}

func TestParseTextualToolCalls_LeavesRealAnswersAlone(t *testing.T) {
	for _, content := range []string{
		"",
		"Got it — queued for check-in (#102280).",
		// Prose that mentions the syntax is an answer about tool calls, not one.
		"History renders calls as [tool_use: exec, args: {}] which is confusing.",
		// A block plus prose is not a pure tool-call response.
		"[tool_use: exec, args: {\"a\":1}]\nAnd then I will check the result.",
		// Malformed args must not execute anything.
		"[tool_use: exec, args: {not json}]",
		"[tool_use: exec, args: \"run it\"]",
	} {
		if calls := parseTextualToolCalls(content); calls != nil {
			t.Errorf("content %q: expected nil, got %d calls", content, len(calls))
		}
	}
}
