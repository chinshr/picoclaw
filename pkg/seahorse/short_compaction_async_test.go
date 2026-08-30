package seahorse

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

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

// newSlowCompletionEngine returns an engine whose summariser blocks for `delay`
// and counts its calls, which is how "did this block the caller" is measured.
func newSlowCompletionEngine(t *testing.T, delay time.Duration) (*CompactionEngine, int64, func() int64) {
	t.Helper()
	ce, _, convID := newTestCompactionEngine(t)
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
	return ce, convID, func() int64 { return atomic.LoadInt64(&calls) }
}

// The whole point of finding 02: the post-turn summarize pass must not make the
// caller wait for an LLM round trip. Compact must return while the summariser is
// still running.
func TestCompactAsyncReturnsWhileLeafStillRunning(t *testing.T) {
	ce, convID, calls := newSlowCompletionEngine(t, 400*time.Millisecond)
	budget := 100

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
	waitFor(t, 3*time.Second, func() bool { return calls() > 0 },
		"the async leaf summariser to actually run")
}

// The proactive and retry passes exist to have shrunk the context by the time
// they return. They must keep blocking.
func TestCompactSyncStillBlocks(t *testing.T) {
	ce, convID, calls := newSlowCompletionEngine(t, 300*time.Millisecond)
	budget := 100

	start := time.Now()
	if _, err := ce.Compact(context.Background(), convID, CompactInput{
		Budget: &budget,
		Async:  false,
	}); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if time.Since(start) < 250*time.Millisecond {
		t.Fatal("sync Compact returned before the summariser finished")
	}
	if calls() == 0 {
		t.Fatal("sync Compact did not run the summariser at all")
	}
}

// A second turn arriving while a leaf summary is still in flight must skip, not
// start a concurrent compaction of the same conversation.
func TestCompactAsyncDedupsConcurrentLeaves(t *testing.T) {
	ce, convID, calls := newSlowCompletionEngine(t, 400*time.Millisecond)
	budget := 100

	for i := 0; i < 5; i++ {
		if _, err := ce.Compact(context.Background(), convID, CompactInput{
			Budget: &budget,
			Async:  true,
		}); err != nil {
			t.Fatalf("Compact #%d: %v", i, err)
		}
	}

	waitFor(t, 3*time.Second, func() bool { return calls() > 0 }, "the first leaf to start")
	if got := calls(); got != 1 {
		t.Fatalf("summariser ran %d times for 5 overlapping compactions, want 1", got)
	}
}

// The goroutine must not inherit the turn context: that is cancelled at turn
// teardown, which would kill the summary a moment after the reply ships — the
// exact thing this change moves it off the path of.
func TestCompactAsyncSurvivesCallerContextCancellation(t *testing.T) {
	ce, convID, calls := newSlowCompletionEngine(t, 300*time.Millisecond)
	budget := 100

	turnCtx, cancelTurn := context.WithCancel(context.Background())
	if _, err := ce.Compact(turnCtx, convID, CompactInput{Budget: &budget, Async: true}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	cancelTurn() // turn teardown, immediately after the reply ships

	waitFor(t, 3*time.Second, func() bool { return calls() > 0 },
		"the leaf summariser to run despite the turn context being cancelled")

	// And it must complete, not abort partway.
	waitFor(t, 3*time.Second, func() bool {
		_, inFlight := ce.compactingLeaf.Load(convID)
		return !inFlight
	}, "the async leaf to finish")
}

// Once the in-flight leaf finishes, the next turn may start a fresh one.
func TestCompactAsyncReleasesDedupAfterCompletion(t *testing.T) {
	ce, convID, calls := newSlowCompletionEngine(t, 50*time.Millisecond)
	budget := 100

	if _, err := ce.Compact(context.Background(), convID, CompactInput{Budget: &budget, Async: true}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool {
		_, inFlight := ce.compactingLeaf.Load(convID)
		return !inFlight
	}, "the first leaf to finish")

	first := calls()
	if _, err := ce.Compact(context.Background(), convID, CompactInput{Budget: &budget, Async: true}); err != nil {
		t.Fatalf("Compact (second): %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return calls() > first },
		"a second leaf to run after the first released the dedup slot")
}
