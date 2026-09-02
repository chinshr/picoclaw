package voicebridge

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
)

// The voice bridge's notices about its own behaviour ("I gave up waiting and
// apologised", "that reply never played") are statements the next answer has to
// be consistent with, not requests. Arriving as an ordinary message.send they
// started a turn each: one full provider call spent on the bridge talking to
// itself, with the visitor's real question demoted to steering inside it. The
// no_reply flag is what lets the agent loop tell the two apart.
func TestNoReplyPayloadSetsRawKey(t *testing.T) {
	meta := map[string]string{}
	applyTurnMetadata(map[string]any{
		PayloadKeyContent: "[SYSTEM EVENT] voice.turn.timeout {}",
		PayloadKeyNoReply: true,
	}, meta)

	if meta[bus.RawKeyNoReply] != "1" {
		t.Fatalf("RawKeyNoReply = %q, want \"1\"", meta[bus.RawKeyNoReply])
	}
}

// A visitor turn must stay a turn. Absent or false is the same thing here:
// anything that is not an explicit no_reply:true runs normally.
func TestOrdinaryPayloadDoesNotSetNoReply(t *testing.T) {
	for name, payload := range map[string]map[string]any{
		"absent": {PayloadKeyContent: "what's your name?"},
		"false":  {PayloadKeyContent: "what's your name?", PayloadKeyNoReply: false},
		"string": {PayloadKeyContent: "what's your name?", PayloadKeyNoReply: "true"},
	} {
		meta := map[string]string{}
		applyTurnMetadata(payload, meta)
		if _, ok := meta[bus.RawKeyNoReply]; ok {
			t.Fatalf("%s: RawKeyNoReply set on an ordinary turn", name)
		}
	}
}

// no_reply is orthogonal to speculation; setting one must not disturb the other.
func TestSpeculativeFlagsStillThreadThrough(t *testing.T) {
	meta := map[string]string{}
	applyTurnMetadata(map[string]any{
		PayloadKeySpeculative:   true,
		PayloadKeySpeculationID: "spec-7",
	}, meta)

	if meta[bus.RawKeySpeculative] != "1" || meta[bus.RawKeySpeculationID] != "spec-7" {
		t.Fatalf("speculative metadata = %v", meta)
	}
	if _, ok := meta[bus.RawKeyNoReply]; ok {
		t.Fatalf("speculative turn marked no_reply")
	}
}

// A speculation id is required: the flag alone would leave the agent with a
// reversible write it can never commit or abort.
func TestSpeculativeWithoutIDIsIgnored(t *testing.T) {
	meta := map[string]string{}
	applyTurnMetadata(map[string]any{
		PayloadKeySpeculative:   true,
		PayloadKeySpeculationID: "   ",
	}, meta)
	if len(meta) != 0 {
		t.Fatalf("metadata = %v, want empty", meta)
	}
}
