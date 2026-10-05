package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"testing"

	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
)

func testMariaDB() *mariaDB {
	client := dynamicfake.NewSimpleDynamicClient(scheme.Scheme)
	// Model successful Kubernetes applies and the deployment controller
	// reaching readiness; the remaining CRUD uses the real fake tracker.
	client.PrependReactor("patch", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		a := action.(k8stesting.PatchAction)
		var object unstructured.Unstructured
		if err := json.Unmarshal(a.GetPatch(), &object); err != nil {
			return true, nil, err
		}
		if object.GetKind() == "Deployment" {
			object.SetGeneration(1)
			object.Object["status"] = map[string]any{"observedGeneration": int64(1), "updatedReplicas": int64(1), "availableReplicas": int64(1), "replicas": int64(1)}
		}
		_, err := client.Tracker().Get(a.GetResource(), a.GetNamespace(), a.GetName())
		if apierrors.IsNotFound(err) {
			err = client.Tracker().Create(a.GetResource(), &object, a.GetNamespace())
		} else if err == nil {
			err = client.Tracker().Update(a.GetResource(), &object, a.GetNamespace())
		}
		return true, &object, err
	})
	return &mariaDB{client: client, namespace: "addons", installation: "main"}
}

func testInstance(space string) *addonv1.Instance {
	return &addonv1.Instance{Installation: "main", Space: space, Name: "db", Offering: "mariadb", Kubernetes: &addonv1.KubernetesContext{
		Namespace: "donkey-" + space, ConsumerLabels: map[string]string{"donkey.local/installation": "main", "donkey.local/space": space}}}
}

func params(t *testing.T, values map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(values)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// captureStream stands in for the gRPC stream: it keeps the progress
// messages and the final result.
type captureStream struct {
	grpc.ServerStream
	ctx      context.Context
	messages []string
	result   *addonv1.ProvisionResponse
}

func (s *captureStream) Context() context.Context { return s.ctx }
func (s *captureStream) Send(event *addonv1.ProvisionEvent) error {
	if event.Message != "" {
		s.messages = append(s.messages, event.Message)
	}
	if event.Result != nil {
		s.result = event.Result
	}
	return nil
}

func provision(ctx context.Context, m *mariaDB, request *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error) {
	stream := &captureStream{ctx: ctx}
	if err := m.Provision(request, stream); err != nil {
		return nil, err
	}
	if stream.result == nil {
		return nil, errors.New("provisioning ended without a result")
	}
	if len(stream.messages) == 0 {
		return nil, errors.New("provisioning reported no progress")
	}
	return stream.result, nil
}

func TestMariaDBDescribesRenderableParameters(t *testing.T) {
	catalog, err := testMariaDB().Offerings(context.Background(), &addonv1.OfferingsRequest{})
	if err != nil || len(catalog.Offerings) != 1 {
		t.Fatalf("catalog: %v %v", catalog, err)
	}
	desc := catalog.Offerings[0]
	properties := desc.Parameters.AsMap()["properties"].(map[string]any)
	if properties["storage"].(map[string]any)["default"] != defaultStorage || properties["version"].(map[string]any)["default"] != defaultVersion {
		t.Fatal("schema defaults disagree with the plugin's own")
	}
	if desc.Id != "mariadb" || desc.Protocol != "mysql" || len(desc.Env) != 6 || !desc.Capabilities.RetainData || !desc.Capabilities.Update {
		t.Fatalf("unexpected descriptor: %+v", desc)
	}
}

func TestMariaDBProvisionAndRetentionLifecycle(t *testing.T) {
	ctx := context.Background()
	m := testMariaDB()
	i := testInstance("team")
	request := &addonv1.ProvisionRequest{Instance: i, Parameters: params(t, map[string]any{"version": "11.4", "storage": "2Gi"}), Reason: addonv1.ProvisionReason_PROVISION_REASON_CREATE,
		SpaceConfiguration: params(t, map[string]any{"storageClass": "fast"})}
	first, err := provision(ctx, m, request)
	if err != nil {
		t.Fatal(err)
	}
	if effective := first.Parameters.AsMap(); effective["storage"] != "2Gi" || effective["version"] != "11.4" {
		t.Fatalf("effective parameters not reported: %v", effective)
	}
	disk, err := m.resource("PersistentVolumeClaim").Get(ctx, instanceName(i), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if class, _, _ := unstructured.NestedString(disk.Object, "spec", "storageClassName"); class != "fast" {
		t.Fatal("admin storage class was not used")
	}
	if _, err := provision(ctx, m, &addonv1.ProvisionRequest{Instance: i, SpaceConfiguration: params(t, map[string]any{"storageClass": "slow"})}); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("existing disk changed storage class:", err)
	}
	request.Reason = addonv1.ProvisionReason_PROVISION_REASON_RETRY
	request.PreviousParameters = first.Parameters
	second, err := provision(ctx, m, request)
	if err != nil || first.Env["MYSQL_PASSWORD"] != second.Env["MYSQL_PASSWORD"] {
		t.Fatalf("repeat provisioning changed credentials: %v", err)
	}
	grown, err := provision(ctx, m, &addonv1.ProvisionRequest{
		Instance: i, Parameters: params(t, map[string]any{"version": "11.4", "storage": "5Gi"}),
		PreviousParameters: first.Parameters, Reason: addonv1.ProvisionReason_PROVISION_REASON_UPDATE,
	})
	if err != nil || grown.Parameters.AsMap()["storage"] != "5Gi" || grown.Env["MYSQL_PASSWORD"] != first.Env["MYSQL_PASSWORD"] {
		t.Fatalf("update failed to grow storage while keeping credentials: %v", err)
	}
	// A disk never shrinks: asking for less keeps the larger volume and says so.
	smaller, err := provision(ctx, m, &addonv1.ProvisionRequest{
		Instance: i, Parameters: params(t, map[string]any{"version": "11.4", "storage": "1Gi"}),
		PreviousParameters: grown.Parameters, Reason: addonv1.ProvisionReason_PROVISION_REASON_UPDATE,
	})
	if err != nil || smaller.Parameters.AsMap()["storage"] != "5Gi" {
		t.Fatalf("volume shrank or effective size lost: %v", err)
	}
	u, err := url.Parse(first.Env["DATABASE_URL"])
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	if password != first.Env["MYSQL_PASSWORD"] || u.Hostname() != first.Endpoints[0].Host || first.Endpoints[0].GetKubernetes().Namespace != "addons" {
		t.Fatal("URL, env and network metadata disagree")
	}
	policy, err := m.resource("NetworkPolicy").Get(ctx, instanceName(i), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var ingress networkingv1.NetworkPolicy
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(policy.Object, &ingress); err != nil {
		t.Fatal(err)
	}
	peers := ingress.Spec.Ingress[0].From
	if len(peers) != 2 || peers[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != i.Kubernetes.Namespace || peers[0].PodSelector.MatchLabels["donkey.local/space"] != i.Space || peers[1].PodSelector.MatchLabels["donkey.local/role"] != "server" {
		t.Fatal("database ingress is not bounded to consumers and admin server")
	}
	other, err := provision(ctx, m, &addonv1.ProvisionRequest{Instance: testInstance("other")})
	if err != nil || other.Endpoints[0].Host == first.Endpoints[0].Host || other.Env["MYSQL_PASSWORD"] == first.Env["MYSQL_PASSWORD"] {
		t.Fatalf("spaces shared an instance: %v", err)
	}
	if _, err := m.Deprovision(ctx, &addonv1.DeprovisionRequest{Instance: i, KeepData: true}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"Secret", "PersistentVolumeClaim"} {
		if _, err := m.resource(kind).Get(ctx, instanceName(i), metav1.GetOptions{}); err != nil {
			t.Fatalf("keep_data lost %s: %v", kind, err)
		}
	}
	for _, kind := range []string{"Deployment", "Service", "NetworkPolicy"} {
		if _, err := m.resource(kind).Get(ctx, instanceName(i), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("keep_data left %s running", kind)
		}
	}
	recreated, err := provision(ctx, m, &addonv1.ProvisionRequest{
		Instance: i, Parameters: grown.Parameters, PreviousParameters: grown.Parameters,
		Reason: addonv1.ProvisionReason_PROVISION_REASON_RESUME,
	})
	if err != nil || recreated.Env["MYSQL_PASSWORD"] != first.Env["MYSQL_PASSWORD"] {
		t.Fatalf("retained database credentials rotated: %v", err)
	}
	if effective := recreated.Parameters.AsMap(); effective["storage"] != "5Gi" || effective["version"] != "11.4" {
		t.Fatalf("resume lost the saved effective parameters: %v", effective)
	}
	for range 2 {
		if _, err := m.Deprovision(ctx, &addonv1.DeprovisionRequest{Instance: i}); err != nil {
			t.Fatal(err)
		}
	}
	for kind := range resources {
		if _, err := m.resource(kind).Get(ctx, instanceName(i), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("full deletion left %s", kind)
		}
		if _, err := m.resource(kind).Get(ctx, instanceName(testInstance("other")), metav1.GetOptions{}); err != nil {
			t.Fatalf("deletion touched another space's %s", kind)
		}
	}
}

func TestMariaDBRefusesCredentialLossAndForeignResources(t *testing.T) {
	ctx := context.Background()
	m := testMariaDB()
	i := testInstance("team")
	if _, err := provision(ctx, m, &addonv1.ProvisionRequest{Instance: i}); err != nil {
		t.Fatal(err)
	}
	if err := m.resource("Secret").Delete(ctx, instanceName(i), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := provision(ctx, m, &addonv1.ProvisionRequest{Instance: i}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("reinitialized credentials for existing data: %v", err)
	}
	if _, err := provision(ctx, m, &addonv1.ProvisionRequest{Instance: i, Parameters: params(t, map[string]any{"version": "bad tag"})}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("accepted an invalid version: %v", err)
	}
	deployment, _ := m.resource("Deployment").Get(ctx, instanceName(i), metav1.GetOptions{})
	deployment.SetLabels(map[string]string{"owner": "somebody-else"})
	if _, err := m.resource("Deployment").Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Deprovision(ctx, &addonv1.DeprovisionRequest{Instance: i}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("deleted a foreign deployment: %v", err)
	}
}
