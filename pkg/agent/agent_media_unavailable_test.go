package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// History keeps media:// refs, but a memory-only store forgets them on restart
// (and the TTL cleaner expires them). The placeholder must not survive as a
// bare "[image: photo 1]", or the model reads it as an attachment it can see.
func TestResolveMediaRefs_UnresolvableRefIsMarkedUnavailable(t *testing.T) {
	store := media.NewFileMediaStore()

	messages := []providers.Message{
		{
			Role:    "user",
			Content: "Checkin this book to the library [image: photo 1]",
			Media:   []string{"media://81951f35-f837-40ba-af90-204dea97c312"},
		},
	}

	result := resolveMediaRefs(messages, store, config.DefaultMaxMediaSize, 0)

	got := result[0].Content
	if !strings.Contains(got, "attachment-unavailable") {
		t.Errorf("expected an unavailable marker, got %q", got)
	}
	if strings.Contains(got, "[image: photo 1]") {
		t.Errorf("generic placeholder should have been replaced, got %q", got)
	}
	if !strings.Contains(got, "Checkin this book to the library") {
		t.Errorf("surrounding text should be preserved, got %q", got)
	}
}

// Same guarantee when the ref resolves but the file was deleted underneath us.
func TestResolveMediaRefs_MissingFileIsMarkedUnavailable(t *testing.T) {
	store := media.NewFileMediaStore()
	dir := t.TempDir()

	path := filepath.Join(dir, "cover.png")
	if err := os.WriteFile(path, []byte("not-really-a-png"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref, err := store.Store(path, media.MediaMeta{Filename: "cover.png"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	messages := []providers.Message{
		{Role: "user", Content: "[image: photo 1]", Media: []string{ref}},
	}

	result := resolveMediaRefs(messages, store, config.DefaultMaxMediaSize, 0)

	if !strings.Contains(result[0].Content, "attachment-unavailable") {
		t.Errorf("expected an unavailable marker, got %q", result[0].Content)
	}
}

// With no placeholder, the message is claiming nothing — so nothing is added.
// Appending here would rewrite content that used to pass through untouched.
func TestResolveMediaRefs_NoPlaceholderContentIsUntouched(t *testing.T) {
	store := media.NewFileMediaStore()

	messages := []providers.Message{
		{Role: "user", Content: "describe this image", Media: []string{"media://image-1"}},
	}

	result := resolveMediaRefs(messages, store, config.DefaultMaxMediaSize, 0)

	if result[0].Content != "describe this image" {
		t.Errorf("content should be unchanged, got %q", result[0].Content)
	}
}

// A resolvable ref must still produce a plain path tag.
func TestInjectPathTags_PathTagStillReplacesPlaceholder(t *testing.T) {
	got := injectPathTags("look [image: photo 1]", []string{"[image:/tmp/x.png]"})
	if got != "look [image:/tmp/x.png]" {
		t.Errorf("got %q", got)
	}
}
