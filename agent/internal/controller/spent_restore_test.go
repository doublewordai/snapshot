// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clientgotesting "k8s.io/client-go/testing"

	"github.com/ai-dynamo/snapshot/api/podcontract"
)

// spentRestorePod is a restored Pod whose workload exited: kubelet restarted
// the container, and the proxy found the restore already used.
func spentRestorePod(reason string) *corev1.Pod {
	pod := restorePod(map[string]string{podcontract.RestoreFromAnnotation: "snapshot-a"})
	status := corev1.ConditionFalse
	if reason == podcontract.RestoreReasonSucceeded {
		status = corev1.ConditionTrue
	}
	pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
		Type: corev1.PodConditionType(podcontract.RestoredCondition), Status: status, Reason: reason,
	})
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
	}
	pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{ExitCode: podcontract.RestoreNotRepeatableExitCode},
	}
	return pod
}

func podDeleted(t *testing.T, w *NodeController, pod *corev1.Pod) bool {
	t.Helper()
	_, err := w.clientset.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true
	}
	require.NoError(t, err)
	return false
}

func TestProcessRestoreQueueItemDeletesPodWhoseRestoreIsSpent(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*corev1.Pod)
	}{
		{
			name:   "restored workload exited",
			mutate: func(*corev1.Pod) {},
		},
		{
			name: "restore failed",
			mutate: func(pod *corev1.Pod) {
				restoredPodCondition(pod).Status = corev1.ConditionFalse
				restoredPodCondition(pod).Reason = podcontract.RestoreReasonFailed
			},
		},
		{
			name: "current instance just exited",
			mutate: func(pod *corev1.Pod) {
				pod.Status.ContainerStatuses[0].State = pod.Status.ContainerStatuses[0].LastTerminationState
				pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
			},
		},
		{
			name: "restore finalizer still present",
			mutate: func(pod *corev1.Pod) {
				pod.Finalizers = []string{restorePodFinalizer}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := spentRestorePod(podcontract.RestoreReasonSucceeded)
			tt.mutate(pod)
			snapshot, content := readySnapshotObjects()
			w := makeTestController(t, pod, snapshot, content)

			processQueuedRestorePod(t, w, pod)

			assert.True(t, podDeleted(t, w, pod))
			event := eventForReason(w.clientset.(*fake.Clientset), spentRestorePodDeletedReason)
			require.NotNil(t, event)
			assert.Equal(t, corev1.EventTypeWarning, event.Type)
			assert.Contains(t, event.Message, "main")
			assert.Contains(t, event.Message, "112")
		})
	}
}

func TestProcessRestoreQueueItemDeletesSpentRestorePodWithItsUID(t *testing.T) {
	pod := spentRestorePod(podcontract.RestoreReasonSucceeded)
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)

	processQueuedRestorePod(t, w, pod)

	var deletes []clientgotesting.DeleteAction
	for _, action := range w.clientset.(*fake.Clientset).Actions() {
		if del, ok := action.(clientgotesting.DeleteAction); ok && del.GetResource().Resource == "pods" {
			deletes = append(deletes, del)
		}
	}
	require.Len(t, deletes, 1)
	preconditions := deletes[0].GetDeleteOptions().Preconditions
	require.NotNil(t, preconditions)
	require.NotNil(t, preconditions.UID)
	assert.Equal(t, pod.UID, *preconditions.UID)
}

func TestProcessRestoreQueueItemKeepsPodWhoseRestoreIsNotSpent(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*NodeController, *corev1.Pod)
	}{
		{
			name: "workload exit code",
			mutate: func(_ *NodeController, pod *corev1.Pod) {
				pod.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = 1
			},
		},
		{
			name: "proxy error exit code",
			mutate: func(_ *NodeController, pod *corev1.Pod) {
				pod.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = podcontract.RestoreProxyErrorExitCode
			},
		},
		{
			name: "no restore requested",
			mutate: func(_ *NodeController, pod *corev1.Pod) {
				delete(pod.Annotations, podcontract.RestoreFromAnnotation)
				pod.Finalizers = []string{restorePodFinalizer}
			},
		},
		{
			name: "exit in a container that is not a restore destination",
			mutate: func(_ *NodeController, pod *corev1.Pod) {
				pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "sidecar"})
				pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
				pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
					Name: "sidecar",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: podcontract.RestoreNotRepeatableExitCode},
					},
				})
			},
		},
		{
			name: "restore not terminal",
			mutate: func(_ *NodeController, pod *corev1.Pod) {
				restoredPodCondition(pod).Status = corev1.ConditionFalse
				restoredPodCondition(pod).Reason = podcontract.RestoreReasonInProgress
			},
		},
		{
			name: "node without the restore proxy",
			mutate: func(w *NodeController, _ *corev1.Pod) {
				w.RestoreWithoutProxy()
			},
		},
		{
			name: "pod already ended",
			mutate: func(_ *NodeController, pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodFailed
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := spentRestorePod(podcontract.RestoreReasonSucceeded)
			snapshot, content := readySnapshotObjects()
			w := makeTestController(t, pod, snapshot, content)
			tt.mutate(w, pod)

			processQueuedRestorePod(t, w, pod)

			assert.False(t, podDeleted(t, w, pod))
			assert.False(t, sawEventReason(w.clientset.(*fake.Clientset), spentRestorePodDeletedReason))
		})
	}
}

// The informer can deliver the Pod again before it sees the deletion. The
// repeat must neither fail nor report a second deletion.
func TestReplaceSpentRestorePodToleratesAPodAlreadyGone(t *testing.T) {
	pod := spentRestorePod(podcontract.RestoreReasonSucceeded)
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	require.NoError(t, w.clientset.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{}))

	handled, retry := w.replaceSpentRestorePod(context.Background(), pod)

	assert.True(t, handled)
	assert.False(t, retry)
	assert.False(t, sawEventReason(w.clientset.(*fake.Clientset), spentRestorePodDeletedReason))
}

func TestReplaceSpentRestorePodRetriesAFailedDeletion(t *testing.T) {
	pod := spentRestorePod(podcontract.RestoreReasonSucceeded)
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)
	w.clientset.(*fake.Clientset).PrependReactor("delete", "pods", func(clientgotesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("apiserver unavailable")
	})

	handled, retry := w.replaceSpentRestorePod(context.Background(), pod)

	assert.True(t, handled)
	assert.True(t, retry)
	assert.False(t, sawEventReason(w.clientset.(*fake.Clientset), spentRestorePodDeletedReason))
}

func TestReplaceSpentRestorePodSkipsADeletingPod(t *testing.T) {
	pod := spentRestorePod(podcontract.RestoreReasonSucceeded)
	now := metav1.Now()
	pod.DeletionTimestamp = &now
	pod.Finalizers = []string{restorePodFinalizer}
	snapshot, content := readySnapshotObjects()
	w := makeTestController(t, pod, snapshot, content)

	handled, _ := w.replaceSpentRestorePod(context.Background(), pod)

	assert.False(t, handled)
	assert.False(t, podDeleted(t, w, pod))
}
