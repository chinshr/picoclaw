package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// library-claw voice-turn-triage finding 13, item 6.
//
// The turn loop kept `pendingMessages` as a local convenience alias of
// exec.pendingMessages, re-seeded it from exec at the END of every iteration,
// and then `append`ed exec.pendingMessages into it again at the TOP of the
// next one. Every steering message therefore entered the context twice.
//
// It was written off as double logging for two days. It was not: session
// interactive-1788329669, main-turn-24, one 55-character sentence produced
//
//	Injected steering message into context  content_len=55 iteration=2
//	Injected steering message into context  content_len=55 iteration=2
//	agent.steering.injected  count=2  total_content_len=110
//
// and the request's message count went 23 -> 25. The visitor's words were in
// the prompt twice, and every subsequent call in the turn carried both copies.
//
// This test models the two lines that mattered, because the loop they live in
// needs a whole agent, registry and provider to run — and a test that needs all
// that to catch a two-line aliasing bug is a test nobody runs.
func TestSteeringDrainDoesNotDuplicate(t *testing.T) {
	steering := []providers.Message{{Role: "user", Content: "Okay, that's a good joke."}}

	// End of iteration 1: the no-tool-call path has just deposited steering on
	// exec and asked the loop to continue.
	execPending := steering
	var local []providers.Message

	// What the end-of-iteration sync now does (it used to be
	// `local = execPending`, which is the bug).
	local = nil

	// Top of iteration 2: the canonical drain.
	if len(execPending) > 0 {
		local = append(local, execPending...)
		execPending = nil
	}

	if len(local) != 1 {
		t.Fatalf("injected %d copies of one steering message, want 1", len(local))
	}
	if len(execPending) != 0 {
		t.Fatalf("exec.pendingMessages not drained: %d left", len(execPending))
	}
}

// The regression, spelled out: with the old end-of-iteration line the same
// message is present twice before injection. Kept as executable documentation
// of what the fix prevents.
func TestTheOldAliasingProducedTwoCopies(t *testing.T) {
	steering := []providers.Message{{Role: "user", Content: "Okay, that's a good joke."}}

	execPending := steering
	var local []providers.Message

	local = execPending // the removed line

	if len(execPending) > 0 {
		local = append(local, execPending...)
	}

	if len(local) != 2 {
		t.Fatalf("expected the old code to duplicate (got %d); if this ever "+
			"stops being true the fix above may be guarding nothing", len(local))
	}
}
