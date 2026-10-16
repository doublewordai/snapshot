// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package types

import "github.com/ai-dynamo/snapshot/agent/pkg/provenance"

// RemoteControlPlaneSpec binds one consumer namespace and virtual node to a
// provider namespace and physical node. The webhook shares the same contract.
type RemoteControlPlaneSpec = provenance.Binding
