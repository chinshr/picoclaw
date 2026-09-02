package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// Finding 11 (voice-turn-triage): a tool turn leaves two junk messages in the
// conversation — messages=20 at iteration 2 where a clean rollback predicts 18,
// plus a duplicate `ingest` of the same visitor sentence.
//
// The finding assumed the rollback was missing. It is not:
// TestSpeculationAbortTruncatesAndRestoresSummary already covers it, and
// pipeline_setup.go snapshots baseLen BEFORE the user message is persisted,
// exactly as it should. So the residue comes from somewhere else, and the
// prime suspect is ordering: the bridge sends `turn.abort` and the re-sent
// transcript back to back on ONE websocket, ~0 ms apart (field 2026-08-31,
// 18:16:49.850 for both). If the re-send is processed first, abort's
// snapshotted baseLen no longer describes the tail it is about to cut.
//
// This pins what must happen in that order. If it ever fails, the race is real
// and this is the bug.

func TestAbortArrivingAfterANewerTurnDoesNotEatIt(t *testing.T) {
	store := newFakeSessionStore()
	const key = "pico:s1"
	store.SetHistory(key, []providers.Message{{Role: "user", Content: "earlier"}})
	store.SetSummary(key, "base summary")

	m := newSpeculationManager()
	m.begin("spec1", key, len(store.GetHistory(key)), store.GetSummary(key))

	// The speculation writes its user message and its (empty) assistant turn,
	// each of which reports itself so abort knows the size of its own block.
	store.AddMessage(key, "user", "how many cars came by")
	m.notePersisted("spec1")
	store.AddMessage(key, "assistant", "")
	m.notePersisted("spec1")

	// The bridge aborts and re-sends in the same millisecond. Here the re-send
	// wins the race: the real turn's user message is already persisted when the
	// abort lands.
	store.AddMessage(key, "user", "how many cars came by")

	m.abort(store, "spec1")

	h := store.GetHistory(key)
	// Whatever else it does, the abort must not leave the REAL user message
	// missing: losing it means the turn answers a question nobody asked.
	last := h[len(h)-1]
	if last.Role != "user" || last.Content != "how many cars came by" {
		t.Fatalf("abort ate the re-sent user message; history=%+v", h)
	}
	// And it must not leave the speculation's copy behind either — that is the
	// duplicate ingest seen in the field.
	users := 0
	for _, msg := range h {
		if msg.Role == "user" && msg.Content == "how many cars came by" {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("expected exactly one copy of the visitor's sentence, got %d: %+v", users, h)
	}
	for _, msg := range h {
		if msg.Role == "assistant" && msg.Content == "" {
			t.Fatalf("the aborted turn's empty assistant message survived: %+v", h)
		}
	}
}
