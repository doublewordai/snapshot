// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package types defines private runtime configuration shared by the operator.
package types

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	DefaultArtifactScanInterval = 10 * time.Minute
	DefaultArtifactBatchSize    = 10
	DefaultArtifactListAttempts = 3
	DefaultArtifactWorkers      = 5
	DefaultArtifactTaskTimeout  = 10 * time.Minute
	DefaultArtifactBackendType  = "pvc"
)

// ArtifactCleanupConfig configures content finalizer cleanup and orphan scans.
type ArtifactCleanupConfig struct {
	// RemoteAgentNodes is a comma-separated set of metadata-plane virtual nodes
	// whose node-local artifacts and cleanup finalizers belong to remote agents.
	RemoteAgentNodes string
	BasePath         string
	ScanInterval     time.Duration
	BatchSize        int
	ListAttempts     int
	Workers          int
	TaskTimeout      time.Duration
	BackendType      string
	// S3 is nil unless the S3 maintenance backend is configured.
	S3 *S3Config
}

func (c ArtifactCleanupConfig) Validate() error {
	if c.RemoteAgentNodes != "" {
		for _, node := range strings.Split(c.RemoteAgentNodes, ",") {
			if errs := validation.IsDNS1123Subdomain(node); len(errs) != 0 {
				return fmt.Errorf("remote agent node %q: %s", node, strings.Join(errs, "; "))
			}
		}
	}
	if c.ScanInterval <= 0 {
		return fmt.Errorf("artifact scan interval must be positive")
	}
	if c.BatchSize <= 0 || c.ListAttempts <= 0 {
		return fmt.Errorf("artifact cleanup integer limits must be positive")
	}
	if c.Workers <= 0 {
		return fmt.Errorf("artifact cleanup worker count must be positive")
	}
	if c.TaskTimeout <= 0 {
		return fmt.Errorf("artifact cleanup task timeout must be positive")
	}
	if c.BackendType == "" {
		return fmt.Errorf("artifact cleanup backend type is required")
	}
	if strings.EqualFold(c.BackendType, "s3") {
		if c.S3 == nil {
			return fmt.Errorf("artifact cleanup s3 config is required when the backend type is s3")
		}
		if err := c.S3.Validate(); err != nil {
			return fmt.Errorf("artifact cleanup s3 config: %w", err)
		}
		return nil
	}
	if c.BasePath == "" {
		return fmt.Errorf("snapshot storage base path is required")
	}
	return nil
}
