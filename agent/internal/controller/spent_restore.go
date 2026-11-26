// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// spentRestorePodDeletedReason is the Event on a restore Pod the agent deleted
// because its one restore is used up and a destination container cannot start
// again.
const spentRestorePodDeletedReason = "SpentRestorePodDeleted"

// replaceSpentRestorePod deletes a restore Pod that can never run again, so
// that its controller replaces it. A Pod gets one restore. Once the restored
// workload exits or the restore fails, every later start of a destination
// container finds the control files left by the earlier one, and its restore
// proxy exits with RestoreNotRepeatableExitCode. kubelet keeps restarting it
// under restartPolicy Always or OnFailure, and the Pod sits in
// CrashLoopBackOff holding its GPUs until someone deletes it.
//
// It acts only on a Pod that requested a restore, whose restore reached a
// terminal outcome, and whose destination container exited with that code.
// Without the proxy the code is the workload's own and says nothing about the
// restore. A Pod that already ended is left to its controller, which
// replaces it without help.
//
// It reports whether it handled the Pod, and whether the deletion has to be
// retried. An unhandled Pod goes through the ordinary restore reconcile.
func (w *NodeController) replaceSpentRestorePod(ctx context.Context, pod *corev1.Pod) (handled, retry bool) {
	if w.withoutProxy || pod.DeletionTimestamp != nil || !isRestorePodActive(pod) ||
		!w.restorePodRequested(pod) || !isRestoreTerminal(pod) {
		return false, false
	}
	exited := notRepeatableContainers(pod)
	if len(exited) == 0 {
		return false, false
	}
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	// The exit code is reserved only in a destination container, where the
	// proxy is PID 1. Resolving the destinations also checks a remote Pod's
	// provenance before the agent deletes it.
	destinations, err := w.RestoreDestinations(ctx, pod)
	if err != nil {
		// The informer resync brings the Pod back, so a transient error only
		// delays the deletion.
		w.log.Error(err, "Cannot resolve restore destinations of a Pod whose restore is spent", "pod", podKey)
		return false, false
	}
	container := ""
	for _, name := range exited {
		if slices.Contains(destinations, name) {
			container = name
			break
		}
	}
	if container == "" {
		return false, false
	}

	message := fmt.Sprintf("Restore destination container %s exited with code %d: this Pod's one restore is spent, so the container cannot run again; deleting the Pod so that its controller replaces it",
		container, podcontract.RestoreNotRepeatableExitCode)
	w.log.Info("Deleting restore Pod whose restore is spent", "pod", podKey, "container", container,
		"exit_code", podcontract.RestoreNotRepeatableExitCode)
	// The UID precondition keeps a replacement that reuses the name, such as a
	// StatefulSet Pod, out of reach.
	err = w.clientset.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &pod.UID},
	})
	switch {
	case err == nil:
		emitPodEvent(ctx, w.clientset, w.log, pod, snapshotEventComponent, corev1.EventTypeWarning, spentRestorePodDeletedReason, message)
		return true, false
	case apierrors.IsNotFound(err) || apierrors.IsConflict(err):
		// The Pod, or this incarnation of it, is already gone.
		return true, false
	default:
		w.log.Error(err, "Failed to delete restore Pod whose restore is spent", "pod", podKey)
		return true, true
	}
}

// notRepeatableContainers returns the containers whose current or previous
// instance exited with RestoreNotRepeatableExitCode.
func notRepeatableContainers(pod *corev1.Pod) []string {
	var names []string
	for _, status := range pod.Status.ContainerStatuses {
		if exitedNotRepeatable(status.State) || exitedNotRepeatable(status.LastTerminationState) {
			names = append(names, status.Name)
		}
	}
	return names
}

func exitedNotRepeatable(state corev1.ContainerState) bool {
	return state.Terminated != nil && state.Terminated.ExitCode == podcontract.RestoreNotRepeatableExitCode
}
