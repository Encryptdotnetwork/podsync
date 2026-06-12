package fs

import (
	"io"
	"os"
	"path/filepath"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// WriteFileAtomic writes the contents of source to path so that readers never
// observe a partially written file: data goes to a temporary file, is flushed to
// disk, then atomically renamed into place. The temporary file is created in the
// destination directory, not os.TempDir, because os.Rename requires both paths to
// be on the same filesystem — temp dirs and data dirs commonly live on different
// Docker volumes (rename across mounts fails with EXDEV).
func WriteFileAtomic(path string, source io.Reader, perm os.FileMode) (written int64, err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return 0, errors.Wrap(err, "failed to create temporary file")
	}

	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			if removeErr := os.Remove(tmpPath); removeErr != nil && !os.IsNotExist(removeErr) {
				log.WithError(removeErr).Errorf("failed to remove temporary file: %s", tmpPath)
			}
		}
	}()

	// os.CreateTemp creates files with 0600, widen to the requested permissions
	if err = tmp.Chmod(perm); err != nil {
		return 0, errors.Wrap(err, "failed to chmod temporary file")
	}

	written, err = io.Copy(tmp, source)
	if err != nil {
		return 0, errors.Wrap(err, "failed to copy data")
	}

	// Flush to disk before rename so a crash can't leave an empty renamed file
	if err = tmp.Sync(); err != nil {
		return 0, errors.Wrap(err, "failed to sync temporary file")
	}

	if err = tmp.Close(); err != nil {
		return 0, errors.Wrap(err, "failed to close temporary file")
	}

	if err = os.Rename(tmpPath, path); err != nil {
		return 0, errors.Wrap(err, "failed to rename temporary file into place")
	}

	return written, nil
}
