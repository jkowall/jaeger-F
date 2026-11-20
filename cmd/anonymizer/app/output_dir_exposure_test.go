// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestDefaultOutputDirIsInsideOperatorHome(t *testing.T) {
	o := Options{}
	c := cobra.Command{}
	o.AddFlags(&c)

	dir := filepath.Clean(o.OutputDir)
	require.True(t, filepath.IsAbs(dir))
	require.NotEqual(t, string(filepath.Separator)+"tmp", dir)
	require.NotEqual(t, filepath.Join(string(filepath.Separator)+"var", "tmp"), dir)
	require.NotEqual(t, filepath.Clean(os.TempDir()), dir)

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	rel, err := filepath.Rel(home, dir)
	require.NoError(t, err)
	outside := rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
	require.False(t, outside, "default output directory %s is outside the operator home %s", dir, home)
}
