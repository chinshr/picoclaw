package channels

import (
	"context"
	"sync"
	"testing"
	"time"
)

// recordingDeleteChannel records DeleteMessage calls from any goroutine.
type recordingDeleteChannel struct {
	mockChannel

	mu      sync.Mutex
	deleted []string
}

func (c *recordingDeleteChannel) DeleteMessage(_ context.Context, _ string, messageID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted = append(c.deleted, messageID)
	return nil
}

func (c *recordingDeleteChannel) deletedSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.deleted...)
}

// Placeholders are tracked per channel+chat. A burst of inbound messages sends
// one placeholder each, but only the last is ever resolved — the earlier
// bubbles used to stay in the chat forever, reading like the bot answered and
// quit. (Field report 2026-08-11: three check-ins, two "Digging…" bubbles that
// were never anything but placeholders.)
func TestRecordPlaceholderDeletesSupersededBubble(t *testing.T) {
	ch := &recordingDeleteChannel{}
	m := newTestManager()
	m.channels["telegram"] = ch

	m.RecordPlaceholder("telegram", "chat-1", "ph-1")
	m.RecordPlaceholder("telegram", "chat-1", "ph-2")
	m.RecordPlaceholder("telegram", "chat-1", "ph-3")

	waitForDeletes(t, ch, 2)

	deleted := ch.deletedSnapshot()
	if deleted[0] != "ph-1" || deleted[1] != "ph-2" {
		t.Errorf("expected ph-1 and ph-2 deleted, got %v", deleted)
	}

	// The newest placeholder stays tracked so the reply can edit or delete it.
	v, ok := m.placeholders.Load("telegram:chat-1")
	if !ok {
		t.Fatal("newest placeholder must remain tracked")
	}
	if entry, ok := v.(placeholderEntry); !ok || entry.id != "ph-3" {
		t.Errorf("expected ph-3 tracked, got %#v", v)
	}
}

// Separate chats must not clean up each other's placeholders.
func TestRecordPlaceholderLeavesOtherChatsAlone(t *testing.T) {
	ch := &recordingDeleteChannel{}
	m := newTestManager()
	m.channels["telegram"] = ch

	m.RecordPlaceholder("telegram", "chat-1", "ph-1")
	m.RecordPlaceholder("telegram", "chat-2", "ph-2")

	time.Sleep(50 * time.Millisecond)
	if got := ch.deletedSnapshot(); len(got) != 0 {
		t.Errorf("no placeholder should have been deleted, got %v", got)
	}
}

// Re-recording the same id is a no-op, not a self-delete.
func TestRecordPlaceholderIgnoresIdenticalID(t *testing.T) {
	ch := &recordingDeleteChannel{}
	m := newTestManager()
	m.channels["telegram"] = ch

	m.RecordPlaceholder("telegram", "chat-1", "ph-1")
	m.RecordPlaceholder("telegram", "chat-1", "ph-1")

	time.Sleep(50 * time.Millisecond)
	if got := ch.deletedSnapshot(); len(got) != 0 {
		t.Errorf("identical placeholder id must not be deleted, got %v", got)
	}
}

func waitForDeletes(t *testing.T, ch *recordingDeleteChannel, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(ch.deletedSnapshot()) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d placeholder deletions, got %v", want, ch.deletedSnapshot())
}
