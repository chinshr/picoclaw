package media

import (
	"os"
	"path/filepath"
)

const TempDirName = "picoclaw_media"

// DirEnvVar overrides where downloaded media is kept.
//
// The default lives under os.TempDir(), which several systems wipe on reboot
// (and systemd-tmpfiles ages out). Point this at a durable directory when
// attachments need to outlive a restart — e.g. a book cover sent to the bot
// that a background worker will OCR later.
const DirEnvVar = "PICOCLAW_MEDIA_DIR"

// TempDir returns the shared directory used for downloaded media.
func TempDir() string {
	if dir := os.Getenv(DirEnvVar); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), TempDirName)
}

// IndexPath returns the on-disk ref index path inside the media directory.
func IndexPath() string {
	return filepath.Join(TempDir(), IndexFileName)
}
