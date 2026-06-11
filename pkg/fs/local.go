package fs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

// LocalConfig is the storage configuration for local file system
type LocalConfig struct {
	DataDir string `toml:"data_dir"`
}

// Local implements local file storage
type Local struct {
	rootDir      string
	WebUIEnabled bool
	NoListing    bool
}

func NewLocal(rootDir string, webUIEnabled bool, noListing bool) (*Local, error) {
	return &Local{rootDir: rootDir, WebUIEnabled: webUIEnabled, NoListing: noListing}, nil
}

func (l *Local) Open(name string) (http.File, error) {
	if name == "/index.html" && l.WebUIEnabled {
		return os.Open("./html/index.html")
	}
	path := filepath.Join(l.rootDir, name)
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	// If NoListing is enabled, prevent directory listings by returning 404
	if l.NoListing {
		stat, err := file.Stat()
		if err != nil {
			file.Close()
			return nil, err
		}
		if stat.IsDir() {
			file.Close()
			return nil, os.ErrNotExist
		}
	}

	return file, nil
}

func (l *Local) Delete(_ctx context.Context, name string) error {
	path := filepath.Join(l.rootDir, name)
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("failed to delete file %s: %w", path, err)
	}
	return nil
}

func (l *Local) Create(_ctx context.Context, name string, reader io.Reader) (int64, error) {
	var (
		logger = log.WithField("name", name)
		path   = filepath.Join(l.rootDir, name)
	)

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return 0, errors.Wrapf(err, "failed to mkdir: %s", path)
	}

	logger.Infof("creating file: %s", path)
	written, err := l.copyFile(reader, path)
	if err != nil {
		return 0, errors.Wrap(err, "failed to copy file")
	}

	logger.Debugf("written %d bytes", written)
	return written, nil
}

// copyFile writes to a temporary file and atomically renames it into place, so
// readers (e.g. podcast clients fetching episodes or the feed XML) never observe
// a partially written file. The temp file is created in the destination directory,
// not os.TempDir, because os.Rename requires both paths to be on the same
// filesystem — temp dirs and data dirs commonly live on different Docker volumes
// (rename across mounts fails with EXDEV).
func (l *Local) copyFile(source io.Reader, destinationPath string) (written int64, err error) {
	tmp, err := os.CreateTemp(filepath.Dir(destinationPath), filepath.Base(destinationPath)+".tmp-*")
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

	// os.CreateTemp creates files with 0600, restore the 0644 os.Create would have used
	if err = tmp.Chmod(0644); err != nil {
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

	if err = os.Rename(tmpPath, destinationPath); err != nil {
		return 0, errors.Wrap(err, "failed to rename temporary file into place")
	}

	return written, nil
}

func (l *Local) Size(_ctx context.Context, name string) (int64, error) {
	file, err := l.Open(name)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return 0, err
	}

	return stat.Size(), nil
}
