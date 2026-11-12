package kubernetes

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestPlatformBlocksDoNotBecomeSourceRepairEvidence(t *testing.T) {
	unschedulable := corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/1 nodes are available: Insufficient cpu"}}}}
	if cause := platformBlock([]corev1.Pod{unschedulable}); !strings.Contains(cause, "Insufficient cpu") {
		t.Fatalf("lost the scheduling cause: %q", cause)
	}
	for _, reason := range []string{"ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError", "CrashLoopBackOff"} {
		pod := corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}}}}
		blocked := platformBlock([]corev1.Pod{pod}) != ""
		if blocked != (reason != "CrashLoopBackOff") {
			t.Errorf("%s: platform block=%v; only application crashes should enter source repair", reason, blocked)
		}
	}
}
