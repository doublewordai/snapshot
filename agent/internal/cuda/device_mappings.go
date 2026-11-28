// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cuda

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// RefuseUncheckpointedDeviceMappings fails when a process of the tree maps an NVIDIA device but is not among the
// CUDA processes cuda-checkpoint will checkpoint. Checkpointing a CUDA process releases its /dev/nvidia* mappings; a
// process forked after CUDA started inherits them without the driver's restore thread, so FilterProcesses skips it.
// CRIU cannot dump such a mapping: it hands it to its device plugins, and AMD's, the only one with that hook, fails
// with "handle_device_vma plugin failed". This names the process before the capture becomes destructive.
func RefuseUncheckpointedDeviceMappings(procRoot string, treePIDs, cudaPIDs []int) error {
	var found []string
	for _, pid := range treePIDs {
		if slices.Contains(cudaPIDs, pid) {
			continue
		}
		device, err := mappedNvidiaDevice(filepath.Join(procRoot, strconv.Itoa(pid)))
		if errors.Is(err, fs.ErrNotExist) {
			continue // exited since the tree was read
		}
		if err != nil {
			return fmt.Errorf("read the mappings of process %d: %w", pid, err)
		}
		if device != "" {
			found = append(found, fmt.Sprintf("%d (%s) maps %s", pid, processName(procRoot, pid), device))
		}
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf("processes map an NVIDIA device without CUDA state to checkpoint, so CRIU cannot dump them: %s; "+
		"a process forked after CUDA started inherits its device mappings (PyTorch's inductor compile workers do: "+
		"set TORCHINDUCTOR_COMPILE_THREADS=1); start such processes with spawn, or before CUDA starts",
		strings.Join(found, "; "))
}

// mappedNvidiaDevice returns the first /dev/nvidia* path in a process's maps, or "" when it maps none.
func mappedNvidiaDevice(processDir string) (string, error) {
	maps, err := os.Open(filepath.Join(processDir, "maps"))
	if err != nil {
		return "", err
	}
	defer maps.Close()
	scanner := bufio.NewScanner(maps)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 6 && strings.HasPrefix(fields[5], "/dev/nvidia") {
			return fields[5], nil
		}
	}
	return "", scanner.Err()
}

// processName returns a process's command line, or its comm when the command line cannot be read.
func processName(procRoot string, pid int) string {
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	if cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil && len(cmdline) > 0 {
		name := strings.TrimSpace(strings.ReplaceAll(string(cmdline), "\x00", " "))
		if len(name) > 120 {
			name = name[:120] + "..."
		}
		return name
	}
	comm, _ := os.ReadFile(filepath.Join(dir, "comm"))
	return strings.TrimSpace(string(comm))
}
