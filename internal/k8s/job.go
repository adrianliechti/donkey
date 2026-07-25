package k8s

import (
	"context"
	"fmt"
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

// jobLogs returns the output of the job's pod — the part a person actually
// needs to see when a migration fails.
func (c *Client) jobLogs(ctx context.Context, namespace, name string) string {
	pods, err := c.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "batch.kubernetes.io/job-name=" + name,
	})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	return c.podLogs(ctx, namespace, pods.Items[0].Name, 200)
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
