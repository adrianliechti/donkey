package k8s

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const rolloutTimeout = 5 * time.Minute

// RolloutEvidence is what donkey collects when a rollout fails: enough for a
// human to see the problem and for the AI assistant to diagnose it.
type RolloutEvidence struct {
	PodSummaries []string
	Events       []string
	PodLogs      string
}

type RolloutError struct {
	App      string
	Evidence RolloutEvidence
	Reason   string
}

func (e *RolloutError) Error() string {
	return fmt.Sprintf("the new version of %s didn't become healthy: %s", e.App, e.Reason)
}

// WaitForRollout polls the Deployment until the new generation is fully
// available, or fails with collected evidence.
func (c *Client) WaitForRollout(ctx context.Context, namespace, name string) error {
	deadline := time.Now().Add(rolloutTimeout)
	for {
		if time.Now().After(deadline) {
			return c.rolloutError(ctx, namespace, name, "it didn't finish starting within 5 minutes")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		dep, err := c.Clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		want := int32(1)
		if dep.Spec.Replicas != nil {
			want = *dep.Spec.Replicas
		}
		st := dep.Status
		// Total replicas must also have converged: otherwise an old pod may
		// still be serving (and answering with stale config) while the new
		// one is just "available enough".
		if st.ObservedGeneration >= dep.Generation &&
			st.UpdatedReplicas == want &&
			st.AvailableReplicas == want &&
			st.Replicas == want {
			return nil
		}
		// Fast-fail on clearly stuck pods (crash loops, bad images).
		if reason := c.stuckReason(ctx, namespace, name); reason != "" {
			return c.rolloutError(ctx, namespace, name, reason)
		}
		time.Sleep(2 * time.Second)
	}
}

// stuckReason returns a plain-language reason when a pod of the app is in an
// unrecoverable-looking state.
func (c *Client) stuckReason(ctx context.Context, namespace, app string) string {
	pods, err := c.appPods(ctx, namespace, app)
	if err != nil {
		return ""
	}
	for _, p := range pods.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil {
				switch w.Reason {
				case "CrashLoopBackOff":
					return "the app keeps crashing right after it starts"
				case "ImagePullBackOff", "ErrImagePull":
					return "the cluster couldn't download the app image from the registry"
				case "CreateContainerConfigError":
					return "the app is missing something it needs to start (often an env secret)"
				}
			}
		}
	}
	return ""
}

func (c *Client) rolloutError(ctx context.Context, namespace, app, reason string) *RolloutError {
	ev := RolloutEvidence{}
	if pods, err := c.appPods(ctx, namespace, app); err == nil {
		for _, p := range pods.Items {
			ev.PodSummaries = append(ev.PodSummaries, podSummary(&p))
		}
		if target := newestProblemPod(pods.Items); target != "" {
			ev.PodLogs = c.podLogs(ctx, namespace, target, 100)
		}
	}
	if evs, err := c.Clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "type=Warning",
	}); err == nil {
		for _, e := range evs.Items {
			ev.Events = append(ev.Events, fmt.Sprintf("%s %s: %s", e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Message))
		}
	}
	return &RolloutError{App: app, Evidence: ev, Reason: reason}
}

func (c *Client) appPods(ctx context.Context, namespace, app string) (*corev1.PodList, error) {
	return c.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "donkey.dev/app=" + app,
	})
}

func (c *Client) podLogs(ctx context.Context, namespace, pod string, lines int64) string {
	req := c.Clientset.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{TailLines: &lines})
	rc, err := req.Stream(ctx)
	if err != nil {
		// The current container may have nothing yet — try the previous crash.
		prev := true
		rc, err = c.Clientset.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{TailLines: &lines, Previous: prev}).Stream(ctx)
		if err != nil {
			return ""
		}
	}
	defer rc.Close()
	b, _ := io.ReadAll(io.LimitReader(rc, 64*1024))
	return string(b)
}

func newestProblemPod(pods []corev1.Pod) string {
	name := ""
	var newest time.Time
	for _, p := range pods {
		if p.Status.Phase == corev1.PodRunning && podReady(&p) {
			continue
		}
		if p.CreationTimestamp.Time.After(newest) {
			newest = p.CreationTimestamp.Time
			name = p.Name
		}
	}
	return name
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func podSummary(p *corev1.Pod) string {
	state := string(p.Status.Phase)
	var notes []string
	for _, cs := range p.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil {
			notes = append(notes, w.Reason)
		}
		if cs.RestartCount > 0 {
			notes = append(notes, fmt.Sprintf("%d restarts", cs.RestartCount))
		}
	}
	if len(notes) > 0 {
		state += " (" + strings.Join(notes, ", ") + ")"
	}
	return p.Name + ": " + state
}
