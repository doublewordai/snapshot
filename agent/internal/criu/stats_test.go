// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package criu

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/checkpoint-restore/go-criu/v8/stats"
)

// statsImage encodes entry as CRIU writes a statistics image.
func statsImage(t *testing.T, entry *stats.StatsEntry) []byte {
	t.Helper()
	payload, err := entry.MarshalVT()
	if err != nil {
		t.Fatal(err)
	}
	return withHeader(payload)
}

// withHeader prefixes payload with a statistics image's magic numbers and size.
func withHeader(payload []byte) []byte {
	buf := binary.LittleEndian.AppendUint32(nil, stats.ImgServiceMagic)
	buf = binary.LittleEndian.AppendUint32(buf, stats.StatsMagic)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(payload)))
	return append(buf, payload...)
}

func writeStats(t *testing.T, name string, content []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func ptr[T any](v T) *T { return &v }

func TestReadDumpStats(t *testing.T) {
	dir := writeStats(t, stats.StatsDump, statsImage(t, &stats.StatsEntry{Dump: &stats.DumpStatsEntry{
		FreezingTime: ptr[uint32](1500), FrozenTime: ptr[uint32](42_000_000), MemdumpTime: ptr[uint32](3_000_000),
		MemwriteTime: ptr[uint32](30_000_000), PagesScanned: ptr[uint64](900), PagesSkippedParent: ptr[uint64](0),
		PagesWritten: ptr[uint64](800), PagesLazy: ptr[uint64](0),
	}}))
	got, err := ReadDumpStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := &DumpStats{Freezing: 1500 * time.Microsecond, Frozen: 42 * time.Second, MemDump: 3 * time.Second,
		MemWrite: 30 * time.Second, PagesScanned: 900, PagesWritten: 800, WrittenBytes: 800 * pageSize}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dump stats = %+v, want %+v", got, want)
	}
}

func TestReadRestoreStats(t *testing.T) {
	dir := writeStats(t, stats.StatsRestore, statsImage(t, &stats.StatsEntry{Restore: &stats.RestoreStatsEntry{
		PagesCompared: ptr[uint64](0), PagesSkippedCow: ptr[uint64](0), ForkingTime: ptr[uint32](250_000),
		RestoreTime: ptr[uint32](7_500_000), PagesRestored: ptr[uint64](1234),
	}}))
	got, err := ReadRestoreStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := &RestoreStats{Forking: 250 * time.Millisecond, Restore: 7500 * time.Millisecond, PagesRestored: 1234,
		RestoredBytes: 1234 * pageSize}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("restore stats = %+v, want %+v", got, want)
	}
}

// A statistics file that is missing, damaged or of the other kind is an error, never a panic.
func TestReadStatsRejectsBadFiles(t *testing.T) {
	dumpOnly := &stats.StatsEntry{Dump: &stats.DumpStatsEntry{FreezingTime: ptr[uint32](1), FrozenTime: ptr[uint32](1),
		MemdumpTime: ptr[uint32](1), MemwriteTime: ptr[uint32](1), PagesScanned: ptr[uint64](1),
		PagesSkippedParent: ptr[uint64](0), PagesWritten: ptr[uint64](1), PagesLazy: ptr[uint64](0)}}
	valid := statsImage(t, dumpOnly)
	overrun := append([]byte(nil), valid...)
	// Far past the file, and past any spare capacity os.ReadFile leaves in its buffer.
	binary.LittleEndian.PutUint32(overrun[stats.SizeOffset:], 1<<30)
	badMagic := append([]byte(nil), valid...)
	badMagic[0]++

	for _, tc := range []struct {
		name    string
		content []byte
	}{
		{"empty", nil},
		{"shorter than the header", valid[:stats.PayloadOffset-1]},
		{"wrong magic", badMagic},
		{"payload past the end", overrun},
		{"undecodable payload", withHeader([]byte{0xff, 0xff})},
		{"a dump's statistics", valid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeStats(t, stats.StatsRestore, tc.content)
			if got, err := ReadRestoreStats(dir); err == nil {
				t.Fatalf("read %+v from a bad file", got)
			}
		})
	}
	if _, err := ReadDumpStats(t.TempDir()); err == nil {
		t.Fatal("read dump statistics from an empty directory")
	}
}
