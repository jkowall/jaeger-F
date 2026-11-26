// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package privatefile

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeAndClose(t *testing.T, f *os.File, data string) {
	t.Helper()
	_, err := f.WriteString(data)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func TestCreateNewFileIsOwnerOnly(t *testing.T) {
	prev := syscall.Umask(0o022)
	defer syscall.Umask(prev)

	path := filepath.Join(t.TempDir(), "out.json")
	f, err := Create(path)
	require.NoError(t, err)
	writeAndClose(t, f, "data")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "data", string(got))
}

func TestCreateReplacesOwnFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.json")
	require.NoError(t, os.WriteFile(path, []byte("stale content"), 0o644))
	reader, err := os.Open(path)
	require.NoError(t, err)
	defer reader.Close()

	f, err := Create(path)
	require.NoError(t, err)
	writeAndClose(t, f, "new")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))

	// A reader that opened the old file must not see the new content.
	old, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, "stale content", string(old))
}

func TestCreateRefusesSymlink(t *testing.T) {
	for name, target := range map[string]string{"existing": "target", "dangling": "missing"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "target"), []byte("keep"), 0o644))
			link := filepath.Join(dir, "out.json")
			require.NoError(t, os.Symlink(filepath.Join(dir, target), link))

			_, err := Create(link)
			require.ErrorContains(t, err, "not a regular file")
			got, err := os.ReadFile(filepath.Join(dir, "target"))
			require.NoError(t, err)
			assert.Equal(t, "keep", string(got))
			_, err = os.Stat(filepath.Join(dir, "missing"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestCreateDoesNotWriteThroughHardLink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "out.json")
	require.NoError(t, os.WriteFile(target, []byte("keep"), 0o644))
	require.NoError(t, os.Link(target, link))

	f, err := Create(link)
	require.NoError(t, err)
	writeAndClose(t, f, "new")

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(got))
	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
}

func TestCreateRefusesFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.json")
	require.NoError(t, syscall.Mkfifo(path, 0o600))

	_, err := Create(path)
	require.ErrorContains(t, err, "not a regular file")
}

func TestRemoveOwnFileRefusesFileOwnedByAnotherUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.json")
	require.NoError(t, os.WriteFile(path, []byte("keep"), 0o644))

	err := removeOwnFile(path, os.Geteuid()+1)
	require.ErrorContains(t, err, "another user")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(got))
}

func TestCreateMissingDirectory(t *testing.T) {
	_, err := Create(filepath.Join(t.TempDir(), "missing", "out.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
