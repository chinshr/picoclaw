package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// fakeSessionStore is a minimal in-memory session.SessionStore for testing the
// speculationManager's history truncation.
type fakeSessionStore struct {
	history map[string][]providers.Message
	summary map[string]string
	saved   int
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{history: map[string][]providers.Message{}, summary: map[string]string{}}
}

func (f *fakeSessionStore) AddMessage(key, role, content string) {
	f.history[key] = append(f.history[key], providers.Message{Role: role, Content: content})
}
func (f *fakeSessionStore) AddFullMessage(key string, msg providers.Message) {
	f.history[key] = append(f.history[key], msg)
}
func (f *fakeSessionStore) GetHistory(key string) []providers.Message {
	out := make([]providers.Message, len(f.history[key]))
	copy(out, f.history[key])
	return out
}
func (f *fakeSessionStore) GetSummary(key string) string  { return f.summary[key] }
func (f *fakeSessionStore) SetSummary(key, summary string) { f.summary[key] = summary }
func (f *fakeSessionStore) SetHistory(key string, h []providers.Message) {
	cp := make([]providers.Message, len(h))
	copy(cp, h)
	f.history[key] = cp
}
func (f *fakeSessionStore) TruncateHistory(key string, keepLast int) {
	h := f.history[key]
	if keepLast < len(h) {
		f.history[key] = h[len(h)-keepLast:]
	}
}
func (f *fakeSessionStore) Save(key string) error    { f.saved++; return nil }
func (f *fakeSessionStore) ListSessions() []string   { return nil }
func (f *fakeSessionStore) Close() error             { return nil }

func TestSpeculationAbortTruncatesAndRestoresSummary(t *testing.T) {
	store := newFakeSessionStore()
	const key = "pico:s1"
	store.SetHistory(key, []providers.Message{{Role: "user", Content: "earlier"}})
	store.SetSummary(key, "base summary")

	m := newSpeculationManager()
	// Speculative turn starts: snapshot base (len 1, base summary).
	m.begin("spec1", key, len(store.GetHistory(key)), store.GetSummary(key))
	// Speculative turn persisted user + assistant + mutated summary.
	store.AddMessage(key, "user", "provisional")
	store.AddMessage(key, "assistant", "speculative reply")
	store.SetSummary(key, "mutated summary")

	m.abort(store, "spec1")

	h := store.GetHistory(key)
	if len(h) != 1 || h[0].Content != "earlier" {
		t.Fatalf("abort must truncate to pre-turn history, got %d msgs: %+v", len(h), h)
	}
	if store.GetSummary(key) != "base summary" {
		t.Fatalf("abort must restore summary, got %q", store.GetSummary(key))
	}
	// Entry consumed: a second abort is a no-op (no further truncation needed).
	store.AddMessage(key, "user", "next real turn")
	m.abort(store, "spec1")
	if len(store.GetHistory(key)) != 2 {
		t.Fatal("abort of an unknown/consumed spec must not touch history")
	}
}

func TestSpeculationCommitKeepsHistory(t *testing.T) {
	store := newFakeSessionStore()
	const key = "pico:s1"
	m := newSpeculationManager()
	m.begin("spec1", key, 0, "")
	store.AddMessage(key, "user", "provisional")
	store.AddMessage(key, "assistant", "reply")

	m.commit("spec1")

	if len(store.GetHistory(key)) != 2 {
		t.Fatalf("commit must keep the speculative turn, got %d", len(store.GetHistory(key)))
	}
	// Entry dropped: a later abort cannot revert a committed turn.
	m.abort(store, "spec1")
	if len(store.GetHistory(key)) != 2 {
		t.Fatal("abort after commit must be a no-op")
	}
}
