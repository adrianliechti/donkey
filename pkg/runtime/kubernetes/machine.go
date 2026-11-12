package kubernetes

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
)

// Personal machines on Kubernetes — see manifests/machine.go for what one is
// made of. A stopped machine is its disk and nothing else: the Deployment,
// the Service, the route and the Secret holding its credentials go, so there
// is nothing left to reach and nothing left to present.

var _ rt.MachineProvider = (*Runtime)(nil)

// machines is the runtime's machine half. A type of its own, not more
// methods on Runtime: Status and Logs already mean an app's there.
type machines struct{ r *Runtime }

func (r *Runtime) Machines() rt.Machines { return machines{r} }

// machineNamespace is where machines live: the namespace of the one space
// name nobody can take, or the installation's single namespace.
func (r *Runtime) machineNamespace() string { return r.ns(config.MachineSpace) }

func (r *Runtime) machineName(id string) string { return r.name(config.MachineSpace, id) }

func (r *Runtime) machineAPIURL() string {
	return fmt.Sprintf("http://%s.%s.svc", manifests.ServerName, r.opts.SystemNamespace)
}

// machinePackagesURL is the package mirror as a machine reaches it — the
// Service builds use, named in full because a machine lives in a namespace
// of its own.
func (r *Runtime) machinePackagesURL() string {
	return fmt.Sprintf("http://%s.%s.svc", manifests.PackagesAppName, r.opts.SystemNamespace)
}

func (r *Runtime) machineInput(spec rt.MachineSpec) (manifests.MachineInput, error) {
	owner := spec.Email
	if owner == "" {
		owner = spec.Name
	}
	in := manifests.MachineInput{
		ID:               spec.ID,
		Namespace:        r.machineNamespace(),
		SharedNamespace:  r.opts.Placement.Shared(),
		SystemNamespace:  r.opts.SystemNamespace,
		ObjectName:       r.machineName(spec.ID),
		Image:            spec.Image,
		Host:             spec.Host,
		Owner:            spec.Owner,
		OwnerLabel:       owner,
		Env:              rt.MachineEnv(spec, r.machineAPIURL(), r.aiProxyURL(), r.machinePackagesURL()),
		Settings:         spec.Settings,
		RestrictEgress:   spec.Settings.RestrictEgress(),
		ImagePullSecrets: r.opts.AddonImagePullSecrets,
		TLS:              r.opts.TLS,
		ExternalDNS:      r.opts.ExternalDNS,
	}
	if !spec.SignInExpires.IsZero() {
		in.SignInExpires = spec.SignInExpires.UTC().Format(time.RFC3339)
	}
	// The same rule every runtime pull follows: the installation's own
	// credential opens its own registry and no other, and an image from
	// elsewhere uses that host's entry in the mounted Docker config, if any.
	host, username, password, err := r.opts.Registry.ImageCredentials(spec.Image)
	if err != nil {
		return in, err
	}
	in.RegistryHost, in.RegistryUsername, in.RegistryPassword = host, username, password
	return in, nil
}

func (m machines) Start(ctx context.Context, spec rt.MachineSpec) error {
	r := m.r
	if !config.ValidMachineID(spec.ID) {
		return fmt.Errorf("%q is not a machine", spec.ID)
	}
	in, err := r.machineInput(spec)
	if err != nil {
		return err
	}
	objs, err := manifests.MachineObjects(in)
	if err != nil {
		return err
	}
	if err := r.c.Apply(ctx, objs); err != nil {
		return fmt.Errorf("the cluster refused the machine: %v", err)
	}
	// Server-side apply never removes what a new set leaves out: an
	// installation that stopped managing certificates or DNS per hostname
	// retracts the ones it made.
	var obsolete []string
	if !in.TLS.UsesCertManager() {
		obsolete = append(obsolete, "Certificate", "Secret")
	}
	if !in.ExternalDNS.Enabled() {
		obsolete = append(obsolete, "DNSEndpoint")
	}
	return r.c.deleteKindsBySelector(ctx, r.opts.SystemNamespace, obsolete, machineEdgeSelector(spec.ID), false)
}

func machineEdgeSelector(id string) map[string]string {
	return map[string]string{manifests.LabelMachine: id, manifests.LabelRole: manifests.RoleEdge}
}

func (m machines) Stop(ctx context.Context, id string) error {
	r := m.r
	if !config.ValidMachineID(id) {
		return fmt.Errorf("%q is not a machine", id)
	}
	ns := r.machineNamespace()
	// The route first, so the gateway stops sending visitors; then the
	// workload; the Secret with the credentials last, once nothing uses it.
	for _, step := range []struct {
		kind string
		role string
	}{
		{"ConfigMap", manifests.RoleMachineRoute},
		{"Service", manifests.RoleMachine},
		{"Deployment", manifests.RoleMachine},
		{"Secret", manifests.RoleMachineConfig},
		{"Secret", manifests.RoleRegistryAuth},
	} {
		if err := r.c.deleteKindsBySelector(ctx, ns, []string{step.kind},
			map[string]string{manifests.LabelMachine: id, manifests.LabelRole: step.role}, false); err != nil {
			return err
		}
	}
	if err := r.c.deleteKindsBySelector(ctx, r.opts.SystemNamespace,
		[]string{"Certificate", "DNSEndpoint", "Secret"}, machineEdgeSelector(id), false); err != nil {
		return fmt.Errorf("removing the machine's public traffic objects: %w", err)
	}
	return nil
}

func (m machines) Delete(ctx context.Context, id string) error {
	r := m.r
	if err := m.Stop(ctx, id); err != nil {
		return err
	}
	return r.c.deleteKindsBySelector(ctx, r.machineNamespace(), []string{"PersistentVolumeClaim"},
		map[string]string{manifests.LabelMachine: id, manifests.LabelRole: manifests.RoleMachineStorage}, false)
}

func (m machines) Status(ctx context.Context, id string) (*rt.MachineStatus, error) {
	r := m.r
	if !config.ValidMachineID(id) {
		return nil, fmt.Errorf("%q is not a machine", id)
	}
	ns, name := r.machineNamespace(), r.machineName(id)
	st := &rt.MachineStatus{State: rt.MachineAbsent}
	dep, err := r.c.Clientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		claim, err := r.c.Clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, manifests.MachineDataClaimName(name), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return st, nil
		}
		if err != nil {
			return nil, err
		}
		if r.c.requireOwnedObject("PersistentVolumeClaim", ns, claim.Name, claim.Labels,
			manifests.MachineLabels(id, manifests.RoleMachineStorage)) == nil && claim.DeletionTimestamp == nil {
			st.State = rt.MachineStopped
		}
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	identity := manifests.MachineLabels(id, manifests.RoleMachine)
	if err := r.c.requireOwnedObject("Deployment", ns, name, dep.Labels, identity); err != nil {
		return nil, err
	}
	st.State = rt.MachineStarting
	st.Image = dep.Annotations[manifests.AnnotationImage]
	st.Host = dep.Annotations[manifests.AnnotationHost]
	if t, err := time.Parse(time.RFC3339, dep.Annotations[manifests.AnnotationSignInExpires]); err == nil {
		st.SignInExpires = t
	}
	if dep.DeletionTimestamp != nil {
		st.State = rt.MachineStopped
		return st, nil
	}

	pods := r.machinePods(ctx, ns, id)
	for i := range pods {
		pod := &pods[i]
		for _, cs := range pod.Status.ContainerStatuses {
			st.Restarts += cs.RestartCount
		}
		if podReady(pod) {
			st.State = rt.MachineRunning
			if pod.Status.StartTime != nil {
				st.StartedAt = pod.Status.StartTime.Time
			}
		}
	}
	if st.State == rt.MachineRunning {
		return st, nil
	}
	if reason := machineStuckReason(pods); reason != "" {
		st.State = rt.MachineFailed
		st.Message = reason
		if pod := newestProblemPod(pods); pod != "" {
			if tail := strings.TrimSpace(r.c.podLogs(ctx, ns, pod, 20)); tail != "" {
				st.Message += "\n" + tail
			}
		}
		return st, nil
	}
	// A pod that cannot be placed says why in its conditions: no node with
	// room, or a disk that is still being made.
	for i := range pods {
		for _, cond := range pods[i].Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Message != "" {
				st.Message = cond.Message
			}
		}
	}
	return st, nil
}

// machinePods are the live pods of one machine, newest first.
func (r *Runtime) machinePods(ctx context.Context, namespace, id string) []corev1.Pod {
	identity := manifests.MachineLabels(id, manifests.RoleMachine)
	selector := labels.Set(r.c.expectedObjectIdentity(manifests.MachineSelector(id))).String()
	list, err := r.c.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil
	}
	var pods []corev1.Pod
	for _, pod := range list.Items {
		if pod.DeletionTimestamp == nil && r.c.requireOwnedObject("Pod", namespace, pod.Name, pod.Labels, identity) == nil {
			pods = append(pods, pod)
		}
	}
	sort.Slice(pods, func(i, j int) bool {
		return pods[j].CreationTimestamp.Before(&pods[i].CreationTimestamp)
	})
	return pods
}

// machineStuckReason says, in the owner's words, why a machine will not come
// up on its own.
func machineStuckReason(pods []corev1.Pod) string {
	for _, pod := range pods {
		for _, cs := range pod.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil {
				switch w.Reason {
				case "CrashLoopBackOff":
					return "the machine keeps stopping right after it starts"
				case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
					return "the cluster couldn't download the machine image"
				case "CreateContainerConfigError", "CreateContainerError":
					return "the cluster couldn't create the machine's container: " + w.Message
				}
			}
		}
	}
	return ""
}

func (m machines) Logs(ctx context.Context, id string, opts rt.LogOptions, out io.Writer) error {
	r := m.r
	if !config.ValidMachineID(id) {
		return fmt.Errorf("%q is not a machine", id)
	}
	ns := r.machineNamespace()
	pods := r.machinePods(ctx, ns, id)
	if len(pods) == 0 {
		return fmt.Errorf("the machine isn't running")
	}
	plo := &corev1.PodLogOptions{Follow: opts.Follow, TailLines: &opts.TailLines}
	if opts.Since > 0 {
		secs := int64(opts.Since.Seconds())
		plo.SinceSeconds = &secs
	}
	rc, err := r.c.Clientset.CoreV1().Pods(ns).GetLogs(pods[0].Name, plo).Stream(ctx)
	if err != nil {
		fmt.Fprintf(out, "no logs yet (%s)\n", podSummary(&pods[0]))
		return nil
	}
	defer rc.Close()
	_, err = io.Copy(out, rc)
	if ctx.Err() != nil {
		return nil // a cancelled follow is a normal hang-up
	}
	return err
}

// machineAITokens are the model tokens of the machines that are running:
// the ones their environment Secrets hold for this installation's endpoint.
func (r *Runtime) machineAITokens(ctx context.Context) ([]string, error) {
	selector := fmt.Sprintf("%s=%s,%s=%s,%s",
		manifests.LabelInstallation, r.c.installation(),
		manifests.LabelRole, manifests.RoleMachineConfig, manifests.LabelMachine)
	secrets, err := r.c.Clientset.CoreV1().Secrets(r.machineNamespace()).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		// No machines namespace yet, or an installation that predates
		// machines and was never granted it: nobody has a token to check.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var tokens []string
	for _, secret := range secrets.Items {
		id := secret.Labels[manifests.LabelMachine]
		if !config.ValidMachineID(id) || secret.DeletionTimestamp != nil ||
			r.c.requireOwnedObject("Secret", secret.Namespace, secret.Name, secret.Labels,
				manifests.MachineLabels(id, manifests.RoleMachineConfig)) != nil {
			continue
		}
		if token := secret.Data[rt.MachineAIKey]; len(token) > 0 && string(secret.Data["OPENAI_BASE_URL"]) == r.aiProxyURL() {
			tokens = append(tokens, string(token))
		}
	}
	return tokens, nil
}
