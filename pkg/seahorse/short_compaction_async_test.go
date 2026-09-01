package seahorse

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// asyncLeafBudget is deliberately huge. Compact's phase 2 (condensed) fires when
// `tokensBefore > budget`, and it calls the SAME CompleteFn — which would pollute
// the call counts these tests rely on. A budget nothing can exceed keeps phase 2
// out of the picture so each test measures leaf behaviour only.
const asyncLeafBudget = 1_000_000

// seedEvictableMessages appends n messages to the conversation's context. Leaf
// compaction needs more than FreshTailCount (32) of them before any are outside
// the protected tail, with at least LeafMinFanout (8) contiguous — the same
// 40-message shape TestCompactLeaf uses.
func seedEvictableMessages(t *testing.T, s *Store, convID int64, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		m, err := s.AddMessage(ctx, convID, "user", "message content for async compaction test", 100)
		if err != nil {
			t.Fatalf("AddMessage %d: %v", i, err)
		}
		if err := s.AppendContextMessage(ctx, convID, m.ID); err != nil {
			t.Fatalf("AppendContextMessage %d: %v", i, err)
		}
	}
}

// waitFor polls until cond() or the deadline, so async assertions do not race.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitForLeafIdle(t *testing.T, ce *CompactionEngine, convID int64) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool {
		_, inFlight := ce.compactingLeaf.Load(convID)
		return !inFlight
	}, "the async leaf goroutine to finish")
}

// newSlowLeafEngine returns a seeded engine whose summariser blocks for `delay`
// and counts its calls. The block is what makes "did this return early" testable.
func newSlowLeafEngine(
	t *testing.T, delay time.Duration, messages int,
) (*CompactionEngine, *Store, int64, func() int64) {
	t.Helper()
	ce, s, convID := newTestCompactionEngine(t)
	seedEvictableMessages(t, s, convID, messages)

	var calls int64
	ce.complete = func(ctx context.Context, prompt string, opts CompleteOptions) (string, error) {
		atomic.AddInt64(&calls, 1)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return "summary of the compacted chunk", nil
	}
	// Must outlive the shared helper's cleanup, which cancels shutdownCtx and
	// then closes the DB: an async leaf still running there would hit a closed
	// database. t.Cleanup is LIFO, so this runs first.
	t.Cleanup(func() { waitForLeafIdle(t, ce, convID) })

	return ce, s, convID, func() int64 { return atomic.LoadInt64(&calls) }
}

// The whole point of finding 02: the post-turn summarize pass must not make the
// caller wait for an LLM round trip. Compact must return while the summariser is
// still running.
func TestCompactAsyncReturnsWhileLeafStillRunning(t *testing.T) {
	ce, _, convID, calls := newSlowLeafEngine(t, 400*time.Millisecond, 40)
	budget := asyncLeafBudget

	start := time.Now()
	if _, err := ce.Compact(context.Background(), convID, CompactInput{
		Budget: &budget,
		Async:  true,
	}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 200*time.Millisecond {
		t.Fatalf("async Compact blocked for %v — it must not wait on the summariser", elapsed)
	}
	waitFor(t, 5*time.Second, func() bool { return calls() > 0 },
		"the async leaf summariser to actually run")
	waitForLeafIdle(t, ce, convID)
}

// The proactive and retry passes exist to have shrunk the context by the time
// they return. They must keep blocking.
func TestCompactSyncStillBlocks(t *testing.T) {
	ce, _, convID, calls := newSlowLeafEngine(t, 300*time.Millisecond, 40)
	budget := asyncLeafBudget

	start := time.Now()
	result, err := ce.Compact(context.Background(), convID, CompactInput{
		Budget: &budget,
		Async:  false,
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	elapsed := time.Since(start)

	if calls() == 0 {
		t.Fatal("sync Compact did not run the summariser at all — the fixture is not triggering leaf compaction")
	}
	if elapsed < 250*time.Millisecond {
		t.Fatalf("sync Compact returned after %v, before the 300ms summariser finished", elapsed)
	}
	if result == nil || result.LeafSummaries == 0 {
		t.Fatalf("sync Compact should report the leaf summary it made: %+v", result)
	}
}

// A second turn arriving while a leaf summary is still in flight must skip, not
// start a concurrent compaction of the same conversation.
func TestCompactAsyncDedupsConcurrentLeaves(t *testing.T) {
	ce, _, convID, calls := newSlowLeafEngine(t, 400*time.Millisecond, 40)
	budget := asyncLeafBudget

	for i := 0; i < 5; i++ {
		if _, err := ce.Compact(context.Background(), convID, CompactInput{
			Budget: &budget,
			Async:  true,
		}); err != nil {
			t.Fatalf("Compact #%d: %v", i, err)
		}
	}

	waitFor(t, 5*time.Second, func() bool { return calls() > 0 }, "the first leaf to start")
	waitForLeafIdle(t, ce, convID)

	if got := calls(); got != 1 {
		t.Fatalf("summariser ran %d times for 5 overlapping compactions, want 1", got)
	}
}

// The goroutine must not inherit the turn context: that is cancelled at turn
// teardown, which would kill the summary a moment after the reply ships — the
// exact thing this change moves it off the path of.
func TestCompactAsyncSurvivesCallerContextCancellation(t *testing.T) {
	ce, s, convID, calls := newSlowLeafEngine(t, 300*time.Millisecond, 40)
	budget := asyncLeafBudget

	turnCtx, cancelTurn := context.WithCancel(context.Background())
	if _, err := ce.Compact(turnCtx, convID, CompactInput{Budget: &budget, Async: true}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	cancelTurn() // turn teardown, immediately after the reply ships

	waitFor(t, 5*time.Second, func() bool { return calls() > 0 },
		"the leaf summariser to run despite the turn context being cancelled")
	waitForLeafIdle(t, ce, convID)

	// It must have COMPLETED, not aborted partway: the summary has to be in the
	// store, otherwise the context never actually shrinks and the next turn pays.
	items, err := s.GetContextItems(context.Background(), convID)
	if err != nil {
		t.Fatalf("GetContextItems: %v", err)
	}
	for _, item := range items {
		if item.ItemType == "summary" {
			return
		}
	}
	t.Fatal("no summary in context_items — the async leaf did not finish its write")
}

// Once the in-flight leaf finishes, a later turn may start a fresh one. A single
// seeding produces exactly one chunk, so this seeds again to give the second
// compaction something to do — which is what a later turn does in practice.
func TestCompactAsyncReleasesDedupAfterCompletion(t *testing.T) {
	ce, s, convID, calls := newSlowLeafEngine(t, 50*time.Millisecond, 40)
	budget := asyncLeafBudget

	if _, err := ce.Compact(context.Background(), convID, CompactInput{Budget: &budget, Async: true}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return calls() > 0 }, "the first leaf to run")
	waitForLeafIdle(t, ce, convID)
	first := calls()

	seedEvictableMessages(t, s, convID, 40)

	if _, err := ce.Compact(context.Background(), convID, CompactInput{Budget: &budget, Async: true}); err != nil {
		t.Fatalf("Compact (second): %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return calls() > first },
		"a second leaf to run after the first released the dedup slot")
	waitForLeafIdle(t, ce, convID)
}
