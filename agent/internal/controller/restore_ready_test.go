// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ai-dynamo/snapshot/agent/internal/types"
	"github.com/ai-dynamo/snapshot/api/podcontract"
)

func restoredPod(node string, restoredAt, readyAt time.Time, ready corev1.ConditionStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "workers", Name: "worker-0", UID: "uid-0"},
		Spec:       corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
			{Type: corev1.PodConditionType(podcontract.RestoredCondition), Status: corev1.ConditionTrue,
				Reason: podcontract.RestoreReasonSucceeded, LastTransitionTime: metav1.NewTime(restoredAt)},
			{Type: corev1.PodReady, Status: ready, LastTransitionTime: metav1.NewTime(readyAt)},
		}},
	}
}

func TestNoteRestoredPodReady(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{"ready 42 s after the restore", restoredPod("node-a", now.Add(-50*time.Second), now.Add(-8*time.Second), corev1.ConditionTrue),
			`"level"=0 "msg"="Restored Pod ready" "pod"="workers/worker-0" "after_restore"="42s"`},
		{"not ready yet", restoredPod("node-a", now.Add(-50*time.Second), now, corev1.ConditionFalse), ""},
		{"another node's Pod", restoredPod("node-b", now.Add(-50*time.Second), now, corev1.ConditionTrue), ""},
		{"ready before the restore finished", restoredPod("node-a", now, now.Add(-time.Second), corev1.ConditionTrue), ""},
		{"ready long ago, as after an agent restart", restoredPod("node-a", now.Add(-2*time.Hour), now.Add(-time.Hour), corev1.ConditionTrue), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lines []string
			w := &NodeController{
				config: &types.AgentConfig{NodeName: "node-a"},
				log:    funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{}),
			}
			w.noteRestoredPodReady(tc.pod)
			w.noteRestoredPodReady(tc.pod)
			got := strings.Join(lines, "\n")
			if got != tc.want {
				t.Fatalf("logged %q, want %q", got, tc.want)
			}
		})
	}
}
