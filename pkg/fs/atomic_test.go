package fs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteFileAtomic(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "config.toml")

	written, err := WriteFileAtomic(path, bytes.NewReader([]byte("a = 1\n")), 0644)
	require.NoError(t, err)
	assert.EqualValues(t, 6, written)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "a = 1\n", string(data))

	// Overwriting an existing file must succeed (rename over destination)
	_, err = WriteFileAtomic(path, bytes.NewReader([]byte("b = 2\n")), 0644)
	require.NoError(t, err)

	data, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "b = 2\n", string(data))

	// Exactly one file in the directory: no temp file litter
	entries, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestWriteFileAtomic_FailedWriteLeavesNoTrace(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "config.toml")

	_, err := WriteFileAtomic(path, errReader{}, 0644)
	require.Error(t, err)

	entries, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "neither destination nor temp file should exist after a failed write")
}
