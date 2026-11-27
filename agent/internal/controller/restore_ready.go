// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// restoredReadyWindow bounds how old a restored Pod's Ready transition may be
// for it to be reported. The set of reported Pods lives in memory, so after an
// agent restart every Pod restored earlier would otherwise be reported again.
const restoredReadyWindow = 10 * time.Minute

// noteRestoredPodReady logs, once per Pod, how long a successfully restored
// Pod took to become Ready after the agent finished its restore. That time is
// the workload's own: waking up after the restore and passing its readiness
// checks. The restore timing summary ends where it begins.
func (w *NodeController) noteRestoredPodReady(obj interface{}) {
	pod, ok := podFromInformerObj(obj)
	if !ok || pod.Spec.NodeName != w.config.NodeName || !isRestoreSucceeded(pod) {
		return
	}
	restored := findRestoredCondition(pod)
	var ready *corev1.PodCondition
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == corev1.PodReady {
			ready = &pod.Status.Conditions[i]
		}
	}
	if ready == nil || ready.Status != corev1.ConditionTrue || ready.LastTransitionTime.Before(&restored.LastTransitionTime) ||
		time.Since(ready.LastTransitionTime.Time) > restoredReadyWindow {
		return
	}
	if _, seen := w.reportedReadyRestores.LoadOrStore(string(pod.UID), struct{}{}); seen {
		return
	}
	w.log.Info("Restored Pod ready",
		"pod", pod.Namespace+"/"+pod.Name,
		"after_restore", ready.LastTransitionTime.Sub(restored.LastTransitionTime.Time).String(),
	)
}
