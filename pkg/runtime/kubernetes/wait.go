package kubernetes

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

const rolloutTimeout = 5 * time.Minute

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
		// Fast-fail on clearly stuck pods (crash loops, bad images) — but only
		// pods of the generation being rolled out: an old copy still crashing
		// must not condemn the fix that is replacing it.
		if reason := stuckReason(c.rolloutPods(ctx, dep)); reason != "" {
			return c.rolloutError(ctx, namespace, name, reason)
		}
		time.Sleep(2 * time.Second)
	}
}

// rolloutPods returns the live pods of the deployment's current generation,
// found via the deployment's own selector (correct for apps and addons alike)
// and narrowed to the newest template hash when it can be determined.
func (c *Client) rolloutPods(ctx context.Context, dep *appsv1.Deployment) []corev1.Pod {
	sel := labels.Set(dep.Spec.Selector.MatchLabels).String()
	if hash := c.currentTemplateHash(ctx, dep, sel); hash != "" {
		sel += ",pod-template-hash=" + hash
	}
	pods, err := c.Clientset.CoreV1().Pods(dep.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return nil
	}
	var out []corev1.Pod
	for _, p := range pods.Items {
		if p.DeletionTimestamp == nil {
			out = append(out, p)
		}
	}
	return out
}

// currentTemplateHash finds the pod-template-hash of the ReplicaSet that
// belongs to the deployment's latest revision. "" when it can't be determined
// (e.g. the first ReplicaSet hasn't been created yet) — all live pods count
// then.
func (c *Client) currentTemplateHash(ctx context.Context, dep *appsv1.Deployment, sel string) string {
	const revisionAnn = "deployment.kubernetes.io/revision"
	if dep.Annotations[revisionAnn] == "" {
		return ""
	}
	rss, err := c.Clientset.AppsV1().ReplicaSets(dep.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
	if err != nil {
		return ""
	}
	for _, rs := range rss.Items {
		if rs.Annotations[revisionAnn] == dep.Annotations[revisionAnn] {
			return rs.Labels["pod-template-hash"]
		}
	}
	return ""
}

// stuckReason returns a plain-language reason when a pod is in an
// unrecoverable-looking state.
func stuckReason(pods []corev1.Pod) string {
	for _, p := range pods {
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

func (c *Client) rolloutError(ctx context.Context, namespace, app, reason string) *rt.RolloutError {
	ev := rt.Evidence{}
	var pods []corev1.Pod
	if dep, err := c.Clientset.AppsV1().Deployments(namespace).Get(ctx, app, metav1.GetOptions{}); err == nil {
		pods = c.rolloutPods(ctx, dep)
	}
	for _, p := range pods {
		ev.Summaries = append(ev.Summaries, podSummary(&p))
	}
	if target := newestProblemPod(pods); target != "" {
		ev.Logs = c.podLogs(ctx, namespace, target, 100)
	}
	if evs, err := c.Clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "type=Warning",
	}); err == nil {
		for _, e := range evs.Items {
			// Only this app's objects: other apps share the namespace, and
			// their noise would mislead both the user and the AI diagnosis.
			if e.InvolvedObject.Name != app && !strings.HasPrefix(e.InvolvedObject.Name, app+"-") {
				continue
			}
			ev.Events = append(ev.Events, fmt.Sprintf("%s %s: %s", e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Message))
		}
	}
	return &rt.RolloutError{App: app, Evidence: ev, Reason: reason}
}

// appPods lists the pods of the app itself — not its addons, and not the
// short-lived file-browser helper, which would otherwise show up in `donkey
// status` and count towards the app's restarts.
func (c *Client) appPods(ctx context.Context, namespace, app string) (*corev1.PodList, error) {
	return c.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: manifests.LabelApp + "=" + app + "," + manifests.LabelComponent + "=" + manifests.ComponentWeb,
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
