// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import "github.com/ai-dynamo/snapshot/agent/pkg/provenance"

// RemoteControlPlaneSpec binds one consumer namespace and virtual node to a
// provider namespace incarnation. It has no physical-node field: the agent
// enforces physical placement separately, through its own NodeName. The
// webhook shares the same contract.
type RemoteControlPlaneSpec = provenance.Binding
