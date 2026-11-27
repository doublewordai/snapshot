// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPagesImageBytesCountsOnlyPagesImages(t *testing.T) {
	dir := t.TempDir()
	for name, size := range map[string]int{"pages-1.img": 4096, "pages-2.img": 100, "pagemap-1.img": 50, "core-1.img": 70} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := pagesImageBytes(dir); err != nil || got != 4196 {
		t.Fatalf("pagesImageBytes = %d, %v; want 4196", got, err)
	}
}
