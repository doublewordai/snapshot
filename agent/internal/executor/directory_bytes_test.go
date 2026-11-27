// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDirectoryBytesCountsRegularFilesOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{"pages-1.img": 4096, "sub/core.img": 100} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A link to a large file outside the directory is not part of what was staged.
	outside := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(outside, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	got, err := directoryBytes(dir)
	if err != nil || got != 4196 {
		t.Fatalf("directoryBytes = %d, %v; want 4196", got, err)
	}
	if _, err := directoryBytes(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing directory measured without error")
	}
}
