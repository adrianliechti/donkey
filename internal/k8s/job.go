package k8s

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// JobError carries why a one-shot job failed, with the output that explains
// it — the same evidence a failed rollout collects.
type JobError struct {
	Name   string
	Reason string
	Logs   string
}

func (e *JobError) Error() string { return e.Reason }

// RunJob replaces any previous run of the job, starts it, and waits.
//
// A Job's pod template is immutable, so a new deploy cannot patch the old
// one: it is deleted first, and with it the pods of the previous run
// (propagation is foreground so the wait doesn't race a half-deleted job).
func (c *Client) RunJob(ctx context.Context, job *batchv1.Job, timeout time.Duration) error {
	jobs := c.Clientset.BatchV1().Jobs(job.Namespace)
	policy := metav1.DeletePropagationForeground
	if err := jobs.Delete(ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// Wait for it to actually be gone, or the create below races the delete.
	for deadline := time.Now().Add(2 * time.Minute); ; {
		if _, err := jobs.Get(ctx, job.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the previous %s run wouldn't go away", job.Name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return err
	}

	deadline := time.Now().Add(timeout)
	for {
		current, err := jobs.Get(ctx, job.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if current.Status.Succeeded > 0 {
			return nil
		}
		if current.Status.Failed > 0 {
			return &JobError{
				Name:   job.Name,
				Reason: jobFailureReason(current),
				Logs:   c.jobLogs(ctx, job.Namespace, job.Name),
			}
		}
		if time.Now().After(deadline) {
			return &JobError{
				Name:   job.Name,
				Reason: fmt.Sprintf("it was still running after %s", timeout),
				Logs:   c.jobLogs(ctx, job.Namespace, job.Name),
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func jobFailureReason(job *batchv1.Job) string {
	for _, cond := range job.Status.Conditions {
		if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue && cond.Message != "" {
			return cond.Message
		}
	}
	return "it exited with an error"
}

// jobLogs returns what a person needs to see when a job fails: its output —
// or, when there is none, why the container never produced any.
//
// The empty case is not an edge case. A container that dies in exec() (a
// command that isn't a real executable, a missing interpreter, a bad image)
// never runs a line of user code, so stdout is empty and the Job's own
// condition says "Job has reached the specified backoff limit" — which is
// true of every failure and explains none of them. The reason is sitting in
// the pod's container status, and without it the diagnosis has no evidence to
// work from either.
func (c *Client) jobLogs(ctx context.Context, namespace, name string) string {
	pods, err := c.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "batch.kubernetes.io/job-name=" + name,
	})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	// The most recent attempt is the one that matters.
	pod := pods.Items[0]
	for _, p := range pods.Items {
		if p.CreationTimestamp.After(pod.CreationTimestamp.Time) {
			pod = p
		}
	}
	if logs := c.podLogs(ctx, namespace, pod.Name, 200); strings.TrimSpace(logs) != "" {
		return logs
	}
	return containerFailure(&pod)
}

// containerFailure describes why a pod's containers are not running, in the
// words the container runtime used.
func containerFailure(pod *corev1.Pod) string {
	var b strings.Builder
	statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	for _, cs := range statuses {
		switch {
		case cs.State.Terminated != nil && (cs.State.Terminated.Reason != "" || cs.State.Terminated.Message != ""):
			t := cs.State.Terminated
			fmt.Fprintf(&b, "container %q stopped: %s (exit code %d)\n", cs.Name, t.Reason, t.ExitCode)
			if t.Message != "" {
				fmt.Fprintf(&b, "%s\n", strings.TrimSpace(t.Message))
			}
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "":
			w := cs.State.Waiting
			fmt.Fprintf(&b, "container %q did not start: %s\n", cs.Name, w.Reason)
			if w.Message != "" {
				fmt.Fprintf(&b, "%s\n", strings.TrimSpace(w.Message))
			}
		}
	}
	if b.Len() == 0 {
		return strings.TrimSpace(pod.Status.Message)
	}
	return b.String()
}

// DeleteJob removes a one-shot job and its pods.
func (c *Client) DeleteJob(ctx context.Context, namespace, name string) error {
	policy := metav1.DeletePropagationForeground
	err := c.Clientset.BatchV1().Jobs(namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
