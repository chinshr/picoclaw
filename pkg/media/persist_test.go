package media

import (
	"os"
	"path/filepath"
	"testing"
)

// A ref recorded in conversation history must still resolve after the process
// restarts — otherwise the message shows an attachment nobody can open.
func TestPersistenceSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, IndexFileName)
	path := createTempFile(t, dir, "cover.jpg")

	first := NewFileMediaStore()
	if _, err := first.EnablePersistence(indexPath); err != nil {
		t.Fatalf("EnablePersistence failed: %v", err)
	}
	ref, err := first.Store(path, MediaMeta{Filename: "cover.jpg", Source: "telegram"}, "scope1")
	if err != nil {
		t.Fatalf("Store failed: %v", err)
	}

	// A fresh store stands in for the restarted process.
	second := NewFileMediaStore()
	restored, err := second.EnablePersistence(indexPath)
	if err != nil {
		t.Fatalf("EnablePersistence failed: %v", err)
	}
	if restored != 1 {
		t.Fatalf("expected 1 restored ref, got %d", restored)
	}

	resolved, meta, err := second.ResolveWithMeta(ref)
	if err != nil {
		t.Fatalf("ResolveWithMeta after restart failed: %v", err)
	}
	if resolved != path {
		t.Errorf("resolved path = %q, want %q", resolved, path)
	}
	if meta.Filename != "cover.jpg" || meta.Source != "telegram" {
		t.Errorf("meta not restored: %+v", meta)
	}
}

// A ref whose file is gone must not come back as a dangling path.
func TestPersistenceDropsMissingFiles(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, IndexFileName)
	path := createTempFile(t, dir, "gone.jpg")

	first := NewFileMediaStore()
	if _, err := first.EnablePersistence(indexPath); err != nil {
		t.Fatalf("EnablePersistence failed: %v", err)
	}
	ref, err := first.Store(path, MediaMeta{Filename: "gone.jpg"}, "scope1")
	if err != nil {
		t.Fatalf("Store failed: %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("failed to remove file: %v", err)
	}

	second := NewFileMediaStore()
	restored, err := second.EnablePersistence(indexPath)
	if err != nil {
		t.Fatalf("EnablePersistence failed: %v", err)
	}
	if restored != 0 {
		t.Fatalf("expected 0 restored refs, got %d", restored)
	}
	if _, err := second.Resolve(ref); err == nil {
		t.Error("Resolve should fail for a ref whose file is gone")
	}
}

// Releasing a scope must clear the ref from disk too, so a later restart does
// not resurrect it.
func TestPersistenceReleaseIsDurable(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, IndexFileName)
	path := createTempFile(t, dir, "temp.jpg")

	first := NewFileMediaStore()
	if _, err := first.EnablePersistence(indexPath); err != nil {
		t.Fatalf("EnablePersistence failed: %v", err)
	}
	ref, err := first.Store(path, MediaMeta{Filename: "temp.jpg"}, "scope1")
	if err != nil {
		t.Fatalf("Store failed: %v", err)
	}
	if err := first.ReleaseAll("scope1"); err != nil {
		t.Fatalf("ReleaseAll failed: %v", err)
	}

	second := NewFileMediaStore()
	if _, err := second.EnablePersistence(indexPath); err != nil {
		t.Fatalf("EnablePersistence failed: %v", err)
	}
	if _, err := second.Resolve(ref); err == nil {
		t.Error("Resolve should fail for a released ref")
	}
}

func TestTempDirHonoursEnvOverride(t *testing.T) {
	custom := t.TempDir()
	t.Setenv(DirEnvVar, custom)

	if got := TempDir(); got != custom {
		t.Errorf("TempDir() = %q, want %q", got, custom)
	}
	if got, want := IndexPath(), filepath.Join(custom, IndexFileName); got != want {
		t.Errorf("IndexPath() = %q, want %q", got, want)
	}
}
