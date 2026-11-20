// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package privatefile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// Create creates path as a new empty file that only the current user can
// access, and opens it for writing.
//
// The file is always a fresh inode created with O_EXCL and O_NOFOLLOW, so
// writes cannot land in a file planted by someone else or reach a reader that
// opened an earlier file at the same path. If path already holds a regular
// file owned by the current user, it is removed first; anything else at path,
// including a symlink, is refused.
func Create(path string) (*os.File, error) {
	f, err := createExclusive(path)
	if !errors.Is(err, fs.ErrExist) {
		return f, err
	}
	if err := removeOwnFile(path, os.Geteuid()); err != nil {
		return nil, fmt.Errorf("refusing to replace %s: %w", path, err)
	}
	return createExclusive(path)
}

func createExclusive(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
}

// removeOwnFile only decides whether to clear the way for createExclusive;
// if path is swapped after the Lstat, the exclusive create still refuses
// whatever was put there.
func removeOwnFile(path string, uid int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case !info.Mode().IsRegular():
		return errors.New("not a regular file")
	case !ok || int64(st.Uid) != int64(uid):
		return errors.New("file is owned by another user")
	}
	return os.Remove(path)
}
