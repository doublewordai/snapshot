// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeProcess(t *testing.T, procRoot string, pid int, cmdline, maps string) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "maps"), []byte(maps), 0600))
}

const (
	pythonMapping = "00400000-00420000 r--p 00000000 00:51 8731341 /usr/bin/python3.12\n"
	deviceMapping = "77e386db0000-77e386dc0000 -w-s 00000000 00:ed 19 /dev/nvidia5\n"
)

func TestRefuseUncheckpointedDeviceMappings(t *testing.T) {
	procRoot := t.TempDir()
	// 10 and 11 are CUDA processes cuda-checkpoint checkpoints; 12 maps no device.
	writeProcess(t, procRoot, 10, "python3\x00-m\x00dynamo.sglang\x00", pythonMapping+deviceMapping)
	writeProcess(t, procRoot, 11, "python3\x00", deviceMapping)
	writeProcess(t, procRoot, 12, "python3\x00", pythonMapping)
	require.NoError(t, RefuseUncheckpointedDeviceMappings(procRoot, []int{10, 11, 12, 99}, []int{10, 11}))

	// 13, an inductor compile worker forked after CUDA started, holds a device mapping but is no CUDA process.
	writeProcess(t, procRoot, 13, "python3\x00-m\x00torch._inductor.compile_worker\x00--kind=fork\x00", pythonMapping+deviceMapping)
	err := RefuseUncheckpointedDeviceMappings(procRoot, []int{10, 11, 12, 13}, []int{10, 11})
	require.ErrorContains(t, err, "13 (python3 -m torch._inductor.compile_worker --kind=fork) maps /dev/nvidia5")
	require.ErrorContains(t, err, "TORCHINDUCTOR_COMPILE_THREADS=1")
	require.NotContains(t, err.Error(), "10 (")
}
