package media

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// IndexFileName is the on-disk ref index written inside the media directory.
const IndexFileName = "refs.json"

// persistedEntry is one ref -> file mapping as written to disk.
type persistedEntry struct {
	Ref           string    `json:"ref"`
	Path          string    `json:"path"`
	Scope         string    `json:"scope"`
	Filename      string    `json:"filename,omitempty"`
	ContentType   string    `json:"content_type,omitempty"`
	Source        string    `json:"source,omitempty"`
	CleanupPolicy string    `json:"cleanup_policy,omitempty"`
	StoredAt      time.Time `json:"stored_at"`
}

type persistedIndex struct {
	Version int              `json:"version"`
	Entries []persistedEntry `json:"entries"`
}

// EnablePersistence points the store at an on-disk index and loads whatever is
// already there. Without it the ref map lives only in memory, so every
// media:// ref recorded in conversation history becomes permanently
// unresolvable the moment the process restarts — the message still shows an
// attachment, but nothing can open it.
//
// Entries whose file no longer exists are dropped during load, so a cleared
// media directory degrades to "no refs" rather than to dangling paths.
// Returns the number of refs restored.
func (s *FileMediaStore) EnablePersistence(indexPath string) (int, error) {
	if indexPath == "" {
		return 0, nil
	}
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o700); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.indexPath = indexPath
	loaded := s.loadIndexLocked(indexPath)
	// Rewrite immediately so entries dropped during load do not come back.
	s.persistLocked()
	return loaded, nil
}

// loadIndexLocked restores refs from disk. Caller holds s.mu.
func (s *FileMediaStore) loadIndexLocked(indexPath string) int {
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.WarnCF("media", "persist: failed to read index", map[string]any{
				"path":  indexPath,
				"error": err.Error(),
			})
		}
		return 0
	}

	var index persistedIndex
	if err := json.Unmarshal(raw, &index); err != nil {
		logger.WarnCF("media", "persist: failed to parse index", map[string]any{
			"path":  indexPath,
			"error": err.Error(),
		})
		return 0
	}

	loaded := 0
	for _, e := range index.Entries {
		if e.Ref == "" || e.Path == "" {
			continue
		}
		if _, exists := s.refs[e.Ref]; exists {
			continue
		}
		if _, err := os.Stat(e.Path); err != nil {
			continue
		}

		meta := MediaMeta{
			Filename:      e.Filename,
			ContentType:   e.ContentType,
			Source:        e.Source,
			CleanupPolicy: normalizeCleanupPolicy(CleanupPolicy(e.CleanupPolicy)),
		}
		storedAt := e.StoredAt
		if storedAt.IsZero() {
			storedAt = s.nowFunc()
		}

		s.refs[e.Ref] = mediaEntry{path: e.Path, meta: meta, storedAt: storedAt}
		if s.scopeToRefs[e.Scope] == nil {
			s.scopeToRefs[e.Scope] = make(map[string]struct{})
		}
		s.scopeToRefs[e.Scope][e.Ref] = struct{}{}
		s.refToScope[e.Ref] = e.Scope
		s.refToPath[e.Ref] = e.Path

		pathState := s.pathStates[e.Path]
		if pathState.refCount == 0 {
			pathState.deleteEligible = meta.CleanupPolicy == CleanupPolicyDeleteOnCleanup
		} else if meta.CleanupPolicy == CleanupPolicyForgetOnly {
			pathState.deleteEligible = false
		}
		pathState.refCount++
		s.pathStates[e.Path] = pathState

		loaded++
	}

	if loaded > 0 {
		logger.InfoCF("media", "persist: restored media refs", map[string]any{
			"path":  indexPath,
			"count": loaded,
		})
	}
	return loaded
}

// persistLocked writes the current ref map to disk atomically. Caller holds
// s.mu. Persistence is best effort: a write failure is logged, never fatal.
func (s *FileMediaStore) persistLocked() {
	if s.indexPath == "" {
		return
	}

	index := persistedIndex{Version: 1, Entries: make([]persistedEntry, 0, len(s.refs))}
	for ref, entry := range s.refs {
		index.Entries = append(index.Entries, persistedEntry{
			Ref:           ref,
			Path:          entry.path,
			Scope:         s.refToScope[ref],
			Filename:      entry.meta.Filename,
			ContentType:   entry.meta.ContentType,
			Source:        entry.meta.Source,
			CleanupPolicy: string(entry.meta.CleanupPolicy),
			StoredAt:      entry.storedAt,
		})
	}

	data, err := json.Marshal(index)
	if err != nil {
		logger.WarnCF("media", "persist: failed to encode index", map[string]any{
			"error": err.Error(),
		})
		return
	}

	tmp := s.indexPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		logger.WarnCF("media", "persist: failed to write index", map[string]any{
			"path":  tmp,
			"error": err.Error(),
		})
		return
	}
	if err := os.Rename(tmp, s.indexPath); err != nil {
		logger.WarnCF("media", "persist: failed to replace index", map[string]any{
			"path":  s.indexPath,
			"error": err.Error(),
		})
		_ = os.Remove(tmp)
	}
}
