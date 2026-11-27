// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/checkpoint-restore/go-criu/v8/stats"
)

// DumpStats is what CRIU recorded about a dump in its stats-dump file.
type DumpStats struct {
	// Freezing is the time CRIU took to freeze the process tree; Frozen is how
	// long the tree then stayed frozen, which covers the rest of the dump.
	Freezing time.Duration `json:"freezing"`
	Frozen   time.Duration `json:"frozen"`
	// MemDump is the time spent collecting memory pages, MemWrite the time
	// spent writing them to the images.
	MemDump  time.Duration `json:"memDump"`
	MemWrite time.Duration `json:"memWrite"`

	PagesScanned uint64 `json:"pagesScanned"`
	PagesWritten uint64 `json:"pagesWritten"`
	// WrittenBytes is PagesWritten in bytes, at this kernel's page size.
	WrittenBytes uint64 `json:"writtenBytes"`
}

// RestoreStats is what CRIU recorded about a restore in its stats-restore file.
type RestoreStats struct {
	// Forking is the time CRIU took to recreate the process tree; Restore is
	// the whole restore, from start until the tasks resumed.
	Forking time.Duration `json:"forking"`
	Restore time.Duration `json:"restore"`

	PagesRestored uint64 `json:"pagesRestored"`
	// RestoredBytes is PagesRestored in bytes, at this kernel's page size.
	RestoredBytes uint64 `json:"restoredBytes"`
}

// ReadDumpStats reads the stats-dump file CRIU writes into its work
// directory, which is the image directory when no work directory is set.
func ReadDumpStats(dir string) (*DumpStats, error) {
	entry, err := readStats(filepath.Join(dir, stats.StatsDump))
	if err != nil {
		return nil, err
	}
	d := entry.GetDump()
	if d == nil {
		return nil, fmt.Errorf("%s holds no dump statistics", stats.StatsDump)
	}
	return &DumpStats{
		Freezing:     microseconds(d.GetFreezingTime()),
		Frozen:       microseconds(d.GetFrozenTime()),
		MemDump:      microseconds(d.GetMemdumpTime()),
		MemWrite:     microseconds(d.GetMemwriteTime()),
		PagesScanned: d.GetPagesScanned(),
		PagesWritten: d.GetPagesWritten(),
		WrittenBytes: d.GetPagesWritten() * pageSize,
	}, nil
}

// ReadRestoreStats reads the stats-restore file CRIU writes into its work
// directory, which is the image directory when no work directory is set.
func ReadRestoreStats(dir string) (*RestoreStats, error) {
	entry, err := readStats(filepath.Join(dir, stats.StatsRestore))
	if err != nil {
		return nil, err
	}
	r := entry.GetRestore()
	if r == nil {
		return nil, fmt.Errorf("%s holds no restore statistics", stats.StatsRestore)
	}
	return &RestoreStats{
		Forking:       microseconds(r.GetForkingTime()),
		Restore:       microseconds(r.GetRestoreTime()),
		PagesRestored: r.GetPagesRestored(),
		RestoredBytes: r.GetPagesRestored() * pageSize,
	}, nil
}

// readStats decodes a CRIU statistics image: two magic numbers, the payload
// size and the StatsEntry. go-criu's own reader indexes the file without
// checking its length, so a truncated file would panic the agent.
func readStats(path string) (*stats.StatsEntry, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(buf) < stats.PayloadOffset {
		return nil, fmt.Errorf("%s: %d bytes is too short for a CRIU image", path, len(buf))
	}
	if binary.LittleEndian.Uint32(buf[stats.PrimaryMagicOffset:]) != stats.ImgServiceMagic ||
		binary.LittleEndian.Uint32(buf[stats.SecondaryMagicOffset:]) != stats.StatsMagic {
		return nil, fmt.Errorf("%s is not a CRIU statistics image", path)
	}
	size := uint64(binary.LittleEndian.Uint32(buf[stats.SizeOffset:]))
	if size > uint64(len(buf)-stats.PayloadOffset) {
		return nil, fmt.Errorf("%s: payload of %d bytes runs past the end of the file", path, size)
	}
	entry := &stats.StatsEntry{}
	if err := entry.UnmarshalVT(buf[stats.PayloadOffset : stats.PayloadOffset+int(size)]); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return entry, nil
}

// pageSize is the unit of CRIU's page counts. The restore runs on the node
// that took the counts, and some arm64 kernels use 64 KiB pages.
var pageSize = uint64(os.Getpagesize())

// microseconds converts CRIU's timings, which it records in microseconds.
func microseconds(us uint32) time.Duration {
	return time.Duration(us) * time.Microsecond
}
