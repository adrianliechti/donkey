package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
)

type runtimePlugin struct {
	addonv1.UnimplementedAddonPluginServer
	deletions     chan *addonv1.DeprovisionRequest
	requests      chan *addonv1.ProvisionRequest
	failDelete    atomic.Bool
	failProvision atomic.Bool
	omitPolicy    atomic.Bool
}

func (*runtimePlugin) Describe(context.Context, *addonv1.DescribeRequest) (*addonv1.DescribeResponse, error) {
	return &addonv1.DescribeResponse{
		Type: "remote-db", Protocol: "mysql", Title: "Remote DB",
		Parameters: addons.MustStruct(addons.ObjectSchema(map[string]any{
			addons.ParamVersion: addons.VersionParameter("11.8", "release"),
			addons.ParamStorage: addons.StorageParameter("1Gi"),
		})),
		Env:          []*addonv1.EnvVar{{Name: "DATABASE_URL", Secret: true}, {Name: "MYSQL_PASSWORD", Secret: true}},
		Capabilities: []string{addons.CapabilityKeepData},
	}, nil
}

func (p *runtimePlugin) Provision(r *addonv1.ProvisionRequest, stream addonv1.AddonPlugin_ProvisionServer) error {
	select {
	case p.requests <- r:
	default:
	}
	if p.failProvision.Load() {
		return status.Error(codes.Unavailable, "provisioning response lost")
	}
	response := &addonv1.ProvisionResponse{
		Env:  map[string]string{"DATABASE_URL": "mysql://donkey:secret@db.addons.svc:3306/app", "MYSQL_PASSWORD": "secret"},
		Urls: []string{"mysql://db.addons.svc:3306/app"},
		Endpoints: []*addonv1.Endpoint{{Host: "db.addons.svc", Port: 3306, Target: &addonv1.Endpoint_Kubernetes{
			Kubernetes: &addonv1.KubernetesTarget{Namespace: "addons", PodLabels: map[string]string{"instance": r.Instance.Space + "-" + r.Instance.Name}},
		}}},
	}
	if p.omitPolicy.Load() {
		response.Endpoints[0].Target = nil
	}
	if err := stream.Send(&addonv1.ProvisionEvent{Message: "ready"}); err != nil {
		return err
	}
	return stream.Send(&addonv1.ProvisionEvent{Result: response})
}

func (p *runtimePlugin) Deprovision(_ context.Context, r *addonv1.DeprovisionRequest) (*addonv1.DeprovisionResponse, error) {
	if p.failDelete.Load() {
		return nil, status.Error(codes.Unavailable, "plugin unavailable")
	}
	p.deletions <- r
	return &addonv1.DeprovisionResponse{}, nil
}

// lastRequest drains the requests the plugin saw and returns the latest.
func lastRequest(t *testing.T, p *runtimePlugin) *addonv1.ProvisionRequest {
	t.Helper()
	var last *addonv1.ProvisionRequest
	for {
		select {
		case r := <-p.requests:
			last = r
		default:
			if last == nil {
				t.Fatal("no provisioning request reached the plugin")
			}
			return last
		}
	}
}

func connectRuntimePlugin(t *testing.T) *runtimePlugin {
	t.Helper()
	p := &runtimePlugin{deletions: make(chan *addonv1.DeprovisionRequest, 10), requests: make(chan *addonv1.ProvisionRequest, 10)}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	addonv1.RegisterAddonPluginServer(s, p)
	go s.Serve(listener)
	t.Cleanup(s.Stop)
	closePlugins, err := addons.ConnectPlugins(context.Background(), config.AddonPlugins{{Type: "remote-db", Address: listener.Addr().String(), Insecure: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closePlugins)
	return p
}

// Both client flavors share one tracker. This exercises the actual apply,
// ownership checks, secret reads, envFrom updates and policy deletion paths.
func pluginTestClient(t *testing.T) *Client {
	t.Helper()
	cs := fake.NewClientset()
	dyn := dynamicfake.NewSimpleDynamicClient(scheme.Scheme)
	kinds := map[schema.GroupVersionResource]string{}
	for kind, gvr := range gvrFor {
		kinds[gvr] = kind
	}
	dyn.PrependReactor("*", "*", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		gvr, ns := action.GetResource(), action.GetNamespace()
		gvk := gvr.GroupVersion().WithKind(kinds[gvr])
		toUnstructured := func(object k8sruntime.Object, err error) (bool, k8sruntime.Object, error) {
			if err != nil {
				return true, nil, err
			}
			raw, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(object)
			return true, &unstructured.Unstructured{Object: raw}, err
		}
		switch action.GetVerb() {
		case "get":
			a := action.(k8stesting.GetAction)
			return toUnstructured(cs.Tracker().Get(gvr, ns, a.GetName()))
		case "patch":
			a := action.(k8stesting.PatchAction)
			object, err := cs.Tracker().Get(gvr, ns, a.GetName())
			fresh := apierrors.IsNotFound(err)
			if fresh {
				object, err = scheme.Scheme.New(gvk)
			}
			if err != nil {
				return true, nil, err
			}
			if err := json.Unmarshal(a.GetPatch(), object); err != nil {
				return true, nil, err
			}
			if fresh {
				err = cs.Tracker().Create(gvr, object, ns)
			} else {
				err = cs.Tracker().Update(gvr, object, ns)
			}
			return toUnstructured(object, err)
		case "list":
			list, err := cs.Tracker().List(gvr, gvk, ns)
			if err != nil {
				return true, nil, err
			}
			raw, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(list)
			if err != nil {
				return true, nil, err
			}
			u, err := (&unstructured.Unstructured{Object: raw}).ToList()
			return true, u, err
		case "delete-collection":
			a := action.(k8stesting.DeleteCollectionAction)
			if !scheme.Scheme.Recognizes(gvk) {
				return true, nil, nil
			} // optional traffic CRDs
			list, err := cs.Tracker().List(gvr, gvk, ns)
			if err != nil {
				return true, nil, err
			}
			items, err := meta.ExtractList(list)
			if err != nil {
				return true, nil, err
			}
			for _, object := range items {
				metadata, _ := meta.Accessor(object)
				if a.GetListRestrictions().Labels.Matches(labels.Set(metadata.GetLabels())) {
					if err := cs.Tracker().Delete(gvr, ns, metadata.GetName()); err != nil {
						return true, nil, err
					}
				}
			}
			return true, nil, nil
		case "delete":
			a := action.(k8stesting.DeleteAction)
			return true, nil, cs.Tracker().Delete(gvr, ns, a.GetName())
		}
		return false, nil, nil
	})
	return &Client{Clientset: cs, Dynamic: dyn}
}

func TestPluginRuntimeProvisionAttachRetainAndDelete(t *testing.T) {
	p := connectRuntimePlugin(t)
	for _, shared := range []string{"", "workloads"} {
		for _, restricted := range []bool{false, true} {
			t.Run(fmt.Sprintf("shared=%s/restricted=%t", shared, restricted), func(t *testing.T) {
				ctx := context.Background()
				opts := Options{SystemNamespace: "main", Placement: manifests.Placement{NamespacePrefix: "donkey-", SharedNamespace: shared}, IsolateSpaces: true, RestrictEgress: restricted}
				r := NewRuntime(pluginTestClient(t), opts)
				spec := rt.ServiceSpec{Type: "remote-db", Name: "db", Parameters: map[string]any{"version": "11.4"}}
				binding, params, err := r.EnsureService(ctx, "team", spec)
				if err != nil || params["version"] != "11.4" || params["storage"] != "1Gi" || binding["MYSQL_PASSWORD"] != "secret" {
					t.Fatalf("provision: %v %v", params, err)
				}
				if got := lastRequest(t, p).Parameters.AsMap(); got["version"] != "11.4" || got["storage"] != "1Gi" {
					t.Fatalf("plugin received %v", got)
				}
				state, err := r.pluginState(ctx, "team", "db")
				if err != nil || state == nil || len(state.Info.Urls) != 1 || state.Instance.Namespace != r.ns("team") || state.State() != addons.StateReady {
					t.Fatalf("lost provisioning metadata: %v", err)
				}
				appLabels := r.c.expectedObjectIdentity(manifests.Labels("team", "api", manifests.RoleWorkload))
				dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: r.name("team", "api"), Namespace: r.ns("team"), Labels: appLabels}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}}}
				if _, err := r.c.Clientset.AppsV1().Deployments(dep.Namespace).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				if found, err := r.AttachService(ctx, "team", "api", "db", binding); err != nil || !found {
					t.Fatalf("attach: %v", err)
				}
				attached, err := r.ServiceBinding(ctx, "team", "api", "db")
				if err != nil || !reflect.DeepEqual(attached, binding) || len(attached) != 2 {
					t.Fatal("metadata leaked into app environment or binding changed")
				}
				dep, _ = r.c.Clientset.AppsV1().Deployments(dep.Namespace).Get(ctx, dep.Name, metav1.GetOptions{})
				refs := dep.Spec.Template.Spec.Containers[0].EnvFrom
				if len(refs) != 1 || refs[0].SecretRef.Name != manifests.AppBindingSecretName(dep.Name, "db") {
					t.Fatal("app environment does not reference its binding secret")
				}
				policyName := r.name("team", "db") + "-egress"
				_, err = r.c.Clientset.NetworkingV1().NetworkPolicies(r.ns("team")).Get(ctx, policyName, metav1.GetOptions{})
				if restricted && err != nil || !restricted && !apierrors.IsNotFound(err) {
					t.Fatalf("egress policy does not follow isolation settings: %v", err)
				}
				for _, owner := range []string{addons.SharedOwner, "api"} {
					services, err := r.ListServices(ctx, "team", owner)
					if err != nil || len(services) != 1 || services[0].Protocol != "mysql" || services[0].Type != "remote-db" || services[0].Parameters["version"] != "11.4" || services[0].State != "" {
						t.Fatalf("lost service metadata for owner %q: %+v %v", owner, services, err)
					}
				}
				// Recreate the runtime to prove lifecycle identity is persisted.
				r = NewRuntime(r.c, opts)
				if host, port, err := r.ServiceEndpoint(ctx, "team", "db"); err != nil || host != "db.addons.svc" || port != 3306 {
					t.Fatalf("tunnel endpoint: %s:%d %v", host, port, err)
				}
				if _, err := r.RemoveService(ctx, "team", "db", true, false); err != nil {
					t.Fatal(err)
				}
				if deletion := <-p.deletions; !deletion.KeepData || deletion.Instance.Space != "team" {
					t.Fatal("retention lost across RPC")
				}
				state, _ = r.pluginState(ctx, "team", "db")
				if state == nil || !state.Stopped {
					t.Fatal("retained instance lost its lifecycle record")
				}
				services, err := r.ListServices(ctx, "team", addons.SharedOwner)
				if err != nil || len(services) != 1 || services[0].State != addons.StateStopped || services[0].Parameters["version"] != "11.4" {
					t.Fatalf("stopped instance not listed: %+v %v", services, err)
				}
				if err := r.reconcilePluginEgress(ctx, "team"); err != nil {
					t.Fatal(err)
				}
				if _, err := r.c.Clientset.NetworkingV1().NetworkPolicies(r.ns("team")).Get(ctx, policyName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
					t.Fatal("stopped service retained network access")
				}
				// Re-adding without parameters resumes the instance as it was,
				// not with whatever the plugin's default is today.
				if _, params, err := r.EnsureService(ctx, "team", rt.ServiceSpec{Type: "remote-db", Name: "db"}); err != nil || params["version"] != "11.4" {
					t.Fatalf("resume: %v %v", params, err)
				}
				if got := lastRequest(t, p).Parameters.AsMap(); got["version"] != "11.4" {
					t.Fatalf("retained instance re-provisioned with %v", got)
				}
				if _, _, err := r.EnsureService(ctx, "team", rt.ServiceSpec{Type: "remote-db", Name: "db", Parameters: map[string]any{"storage": "5Gi"}}); err == nil {
					t.Fatal("the disk of an existing instance changed")
				}
				p.failDelete.Store(true)
				if _, err := r.RemoveService(ctx, "team", "db", false, false); err == nil {
					t.Fatal("failed plugin deletion was reported successful")
				}
				p.failDelete.Store(false)
				if state, _ := r.pluginState(ctx, "team", "db"); state == nil {
					t.Fatal("failed deletion discarded retry identity")
				}
				if _, err := r.RemoveService(ctx, "team", "db", false, false); err != nil {
					t.Fatal(err)
				}
				if deletion := <-p.deletions; deletion.KeepData {
					t.Fatal("full deletion kept data")
				}
				if state, err := r.pluginState(ctx, "team", "db"); err != nil || state != nil {
					t.Fatalf("deleted service retained state: %v", err)
				}
			})
		}
	}
}

func TestPluginRuntimeKeepsIntentOnLostProvisionResponse(t *testing.T) {
	p := connectRuntimePlugin(t)
	ctx := context.Background()
	r := NewRuntime(pluginTestClient(t), Options{SystemNamespace: "main", Placement: manifests.Placement{SharedNamespace: "workloads"}})
	p.failProvision.Store(true)
	if _, _, err := r.EnsureService(ctx, "team", rt.ServiceSpec{Type: "remote-db", Name: "db"}); err == nil {
		t.Fatal("lost provision response was accepted")
	}
	state, err := r.pluginState(ctx, "team", "db")
	if err != nil || state == nil || state.Info != nil || state.State() != addons.StateFailed || state.Error == "" {
		t.Fatal("failed provisioning must retain intent and its error without publishing credentials")
	}
	services, err := r.ListServices(ctx, "team", addons.SharedOwner)
	if err != nil || len(services) != 1 || services[0].State != addons.StateFailed || services[0].Message == "" || services[0].Type != "remote-db" {
		t.Fatalf("failed instance is invisible: %+v %v", services, err)
	}
	if err := r.DeleteSpace(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	if deletion := <-p.deletions; deletion.KeepData || deletion.Instance.Name != "db" {
		t.Fatal("space deletion did not clean up pending external resources")
	}
}

func TestPluginRuntimeOrphansInstanceWhosePluginIsGone(t *testing.T) {
	p := connectRuntimePlugin(t)
	ctx := context.Background()
	r := NewRuntime(pluginTestClient(t), Options{SystemNamespace: "main", Placement: manifests.Placement{SharedNamespace: "workloads"}})
	if _, _, err := r.EnsureService(ctx, "team", rt.ServiceSpec{Type: "remote-db", Name: "db"}); err != nil {
		t.Fatal(err)
	}
	p.failDelete.Store(true)
	if _, err := r.RemoveService(ctx, "team", "db", false, false); err == nil || !strings.Contains(err.Error(), "--orphan") {
		t.Fatalf("a failing plugin did not point at --orphan: %v", err)
	}
	if _, err := r.RemoveService(ctx, "team", "db", false, true); err != nil {
		t.Fatal(err)
	}
	if state, err := r.pluginState(ctx, "team", "db"); err != nil || state != nil {
		t.Fatal("orphaned instance kept its record")
	}
	if binding, err := r.ServiceBinding(ctx, "team", addons.SharedOwner, "db"); err != nil || len(binding) != 0 {
		t.Fatal("orphaned instance kept its binding")
	}
}

func TestPluginResponseSurvivesPolicyFailure(t *testing.T) {
	p := connectRuntimePlugin(t)
	p.omitPolicy.Store(true)
	ctx := context.Background()
	r := NewRuntime(pluginTestClient(t), Options{SystemNamespace: "main", Placement: manifests.Placement{SharedNamespace: "workloads"}, IsolateSpaces: true, RestrictEgress: true})
	if _, _, err := r.EnsureService(ctx, "team", rt.ServiceSpec{Type: "remote-db", Name: "db"}); err == nil {
		t.Fatal("accepted a destination without an egress target")
	}
	state, err := r.pluginState(ctx, "team", "db")
	if err != nil || state == nil || state.Info == nil || len(state.Info.Endpoints) != 1 || state.State() != addons.StateFailed {
		t.Fatalf("lost successful plugin response or its failure: %v", err)
	}
	if binding, err := r.ServiceBinding(ctx, "team", addons.SharedOwner, "db"); err != nil || len(binding) != 0 {
		t.Fatal("published a binding before its egress policy")
	}
	if _, err := r.RemoveService(ctx, "team", "db", false, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.deletions:
	default:
		t.Fatal("failed instance could not be deprovisioned")
	}
}

func TestSpaceOwnershipCheckedBeforePluginDeletion(t *testing.T) {
	p := connectRuntimePlugin(t)
	ctx := context.Background()
	r := NewRuntime(pluginTestClient(t), Options{SystemNamespace: "main", Placement: manifests.Placement{NamespacePrefix: "donkey-"}})
	if _, _, err := r.EnsureService(ctx, "team", rt.ServiceSpec{Type: "remote-db", Name: "db"}); err != nil {
		t.Fatal(err)
	}
	ns, err := r.c.Clientset.CoreV1().Namespaces().Get(ctx, "donkey-team", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ns.Annotations[manifests.AnnotationManaged] = "false"
	if _, err := r.c.Clientset.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteSpace(ctx, "team"); err == nil {
		t.Fatal("deleted resources before checking namespace ownership")
	}
	select {
	case <-p.deletions:
		t.Fatal("deprovisioned the database despite an unowned namespace")
	default:
	}
}

func TestPluginEgressReconcilesInstallationSettingChanges(t *testing.T) {
	connectRuntimePlugin(t)
	ctx := context.Background()
	r := NewRuntime(pluginTestClient(t), Options{SystemNamespace: "main", Placement: manifests.Placement{SharedNamespace: "workloads"}, IsolateSpaces: true, RestrictEgress: true})
	if _, _, err := r.EnsureService(ctx, "team", rt.ServiceSpec{Type: "remote-db", Name: "db"}); err != nil {
		t.Fatal(err)
	}
	for _, restricted := range []bool{false, true, false} {
		r.opts.RestrictEgress = restricted
		if err := r.ReconcileSpaceIsolation(ctx); err != nil {
			t.Fatal(err)
		}
		_, err := r.c.Clientset.NetworkingV1().NetworkPolicies("workloads").Get(ctx, "team-db-egress", metav1.GetOptions{})
		if restricted && err != nil || !restricted && !apierrors.IsNotFound(err) {
			t.Fatalf("stale addon egress policy after setting change: %v", err)
		}
	}
}
