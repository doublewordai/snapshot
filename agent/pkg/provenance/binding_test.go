// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package provenance

import "testing"

// With several fields missing, every start must report the same one, so an
// operator fixing the binding is not chased between messages.
func TestBindingValidateReportsTheFirstInvalidFieldDeterministically(t *testing.T) {
	for range 50 {
		err := Binding{}.Validate()
		if err == nil || err.Error() != "remoteControlPlane.kubeconfig must be nonempty without surrounding whitespace" {
			t.Fatalf("Validate() = %v, want the kubeconfig error first", err)
		}
	}
}
