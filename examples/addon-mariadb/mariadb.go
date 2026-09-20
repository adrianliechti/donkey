package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"sync"
	"time"

	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
)

const databasePort = 3306

var envKeys = []string{"DATABASE_URL", "MYSQL_HOST", "MYSQL_PORT", "MYSQL_DATABASE", "MYSQL_USER", "MYSQL_PASSWORD"}
var imageTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

type mariaDB struct {
	addonv1.UnimplementedAddonPluginServer
	client                  dynamic.Interface
	namespace, installation string
	// The example runs one replica. Serializing mutations also prevents two
	// initial requests from assigning different passwords to the same disk.
	mu sync.Mutex
}

func (m *mariaDB) Describe(context.Context, *addonv1.DescribeRequest) (*addonv1.DescribeResponse, error) {
	return &addonv1.DescribeResponse{
		Protocol: "mysql",
		Type:     "mariadb", Title: "MariaDB", Description: "A MariaDB database provisioned in a separate namespace.",
		DefaultVersion: "11.8", DefaultStorage: "1Gi", EnvKeys: envKeys,
	}, nil
}

func (m *mariaDB) validate(i *addonv1.Instance) error {
	if i == nil || i.Runtime != "kubernetes" {
		return status.Error(codes.InvalidArgument, "this example requires the Kubernetes runtime")
	}
	if i.Installation != m.installation {
		return status.Error(codes.PermissionDenied, "unknown installation")
	}
	for _, value := range []string{i.Installation, i.Space, i.Name, i.Namespace} {
		if value == "" || len(validation.IsDNS1123Label(value)) != 0 {
			return status.Error(codes.InvalidArgument, "invalid instance identity")
		}
	}
	if !maps.Equal(i.ConsumerLabels, map[string]string{"donkey.local/installation": i.Installation, "donkey.local/space": i.Space}) {
		return status.Error(codes.InvalidArgument, "consumer labels must select exactly this installation and space")
	}
	return nil
}

func instanceName(i *addonv1.Instance) string {
	sum := sha256.Sum256([]byte(i.Installation + "/" + i.Space + "/" + i.Name))
	return "mariadb-" + hex.EncodeToString(sum[:12])
}

func instanceLabels(i *addonv1.Instance) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by":     "donkey-mariadb-plugin",
		"addons.donkey.local/installation": i.Installation,
		"addons.donkey.local/space":        i.Space,
		"addons.donkey.local/name":         i.Name,
	}
}

func (m *mariaDB) Provision(ctx context.Context, request *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error) {
	if request == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	if err := m.validate(request.Instance); err != nil {
		return nil, err
	}
	version := request.Version
	if version == "" {
		version = "11.8"
	}
	if !imageTag.MatchString(version) {
		return nil, status.Error(codes.InvalidArgument, "version must be a MariaDB image tag")
	}
	storage := request.Storage
	if storage == "" {
		storage = "1Gi"
	}
	size, err := resource.ParseQuantity(storage)
	if err != nil || size.Sign() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "storage must be a positive Kubernetes quantity")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	i := request.Instance
	name, labels := instanceName(i), instanceLabels(i)
	secret, err := m.resource("Secret").Get(ctx, name, metav1.GetOptions{})
	var password string
	if apierrors.IsNotFound(err) {
		// A surviving volume without its credentials must never be silently
		// assigned a new password: MariaDB ignores initialization env then.
		if _, diskErr := m.resource("PersistentVolumeClaim").Get(ctx, name, metav1.GetOptions{}); diskErr == nil {
			return nil, status.Error(codes.FailedPrecondition, "database disk exists but credentials are missing")
		} else if !apierrors.IsNotFound(diskErr) {
			return nil, diskErr
		}
		password = rand.Text()
		if err := m.apply(ctx, &corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: m.namespace, Labels: labels},
			Data:       map[string][]byte{"password": []byte(password), "root-password": []byte(rand.Text())},
		}); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else {
		if !owned(secret, labels) {
			return nil, status.Error(codes.AlreadyExists, "credential secret has another owner")
		}
		var stored corev1.Secret
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(secret.Object, &stored); err != nil {
			return nil, err
		}
		password = string(stored.Data["password"])
		if password == "" || len(stored.Data["root-password"]) == 0 {
			return nil, status.Error(codes.FailedPrecondition, "database credentials are incomplete")
		}
	}
	if existing, err := m.resource("PersistentVolumeClaim").Get(ctx, name, metav1.GetOptions{}); err == nil {
		if !owned(existing, labels) {
			return nil, status.Error(codes.AlreadyExists, "database disk has another owner")
		}
		var pvc corev1.PersistentVolumeClaim
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(existing.Object, &pvc); err != nil {
			return nil, err
		}
		if current := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; current.Cmp(size) > 0 {
			size = current
		}
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	for _, object := range m.objects(i, version, size) {
		if err := m.apply(ctx, object); err != nil {
			return nil, err
		}
	}
	if err := wait.PollUntilContextTimeout(ctx, time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
		object, err := m.resource("Deployment").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		var dep appsv1.Deployment
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &dep); err != nil {
			return false, err
		}
		return dep.Status.ObservedGeneration >= dep.Generation && dep.Status.UpdatedReplicas == 1 && dep.Status.AvailableReplicas == 1 && dep.Status.Replicas == 1, nil
	}); err != nil {
		return nil, status.Error(codes.DeadlineExceeded, "MariaDB did not become ready before the provisioning deadline")
	}
	host := name + "." + m.namespace + ".svc"
	connection := &url.URL{Scheme: "mysql", User: url.UserPassword("donkey", password), Host: host + ":3306", Path: "/app"}
	return &addonv1.ProvisionResponse{
		Protocol: "mysql",
		Env:      map[string]string{"DATABASE_URL": connection.String(), "MYSQL_HOST": host, "MYSQL_PORT": "3306", "MYSQL_DATABASE": "app", "MYSQL_USER": "donkey", "MYSQL_PASSWORD": password},
		Urls:     []string{"mysql://" + host + ":3306/app"},
		Endpoints: []*addonv1.Endpoint{{Host: host, Port: databasePort, Protocol: "TCP", Kubernetes: &addonv1.KubernetesTarget{
			Namespace: m.namespace, PodLabels: labels, Port: databasePort,
		}}},
	}, nil
}

func (m *mariaDB) objects(i *addonv1.Instance, version string, size resource.Quantity) []runtime.Object {
	name, labels := instanceName(i), instanceLabels(i)
	meta := metav1.ObjectMeta{Name: name, Namespace: m.namespace, Labels: labels}
	tcp, port := corev1.ProtocolTCP, intstr.FromInt(databasePort)
	secretEnv := func(env, key string) corev1.EnvVar {
		return corev1.EnvVar{Name: env, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}}}
	}
	probe := corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"healthcheck.sh", "--connect", "--innodb_initialized"}}}
	return []runtime.Object{
		&corev1.PersistentVolumeClaim{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}, ObjectMeta: meta,
			Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}}},
		},
		// The plugin owns ingress in its namespace. Consumers and the admin
		// server may connect, each bounded to its own namespace AND labels.
		&networkingv1.NetworkPolicy{
			TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"}, ObjectMeta: meta,
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: labels},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}}, From: []networkingv1.NetworkPolicyPeer{
					{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": i.Namespace}}, PodSelector: &metav1.LabelSelector{MatchLabels: maps.Clone(i.ConsumerLabels)}},
					{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": i.Installation}}, PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"donkey.local/installation": i.Installation, "donkey.local/role": "server"}}},
				}}},
			},
		},
		&corev1.Service{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: meta,
			Spec: corev1.ServiceSpec{Selector: labels, Ports: []corev1.ServicePort{{Name: "mysql", Port: databasePort, TargetPort: port}}},
		},
		&appsv1.Deployment{
			TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: meta,
			Spec: appsv1.DeploymentSpec{
				Replicas: new(int32(1)), Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
					AutomountServiceAccountToken: new(false), EnableServiceLinks: new(false),
					SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: new(true), RunAsUser: new(int64(999)), RunAsGroup: new(int64(999)), FSGroup: new(int64(999))},
					Containers: []corev1.Container{{
						Name: "mariadb", Image: "mariadb:" + version, Ports: []corev1.ContainerPort{{Name: "mysql", ContainerPort: databasePort}},
						SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: new(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
						Env:             []corev1.EnvVar{{Name: "MARIADB_DATABASE", Value: "app"}, {Name: "MARIADB_USER", Value: "donkey"}, {Name: "MARIADB_ROOT_HOST", Value: "localhost"}, secretEnv("MARIADB_PASSWORD", "password"), secretEnv("MARIADB_ROOT_PASSWORD", "root-password")},
						VolumeMounts:    []corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/mysql"}},
						ReadinessProbe:  &corev1.Probe{ProbeHandler: probe, PeriodSeconds: 5, TimeoutSeconds: 3},
						StartupProbe:    &corev1.Probe{ProbeHandler: probe, PeriodSeconds: 5, TimeoutSeconds: 3, FailureThreshold: 36},
						Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")}, Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}},
					}},
					Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name}}}},
				}},
			},
		},
	}
}

func (m *mariaDB) Deprovision(ctx context.Context, request *addonv1.DeprovisionRequest) (*addonv1.DeprovisionResponse, error) {
	if request == nil {
		return nil, status.Error(codes.InvalidArgument, "missing request")
	}
	if err := m.validate(request.Instance); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	name, labels := instanceName(request.Instance), instanceLabels(request.Instance)
	kinds := []string{"Deployment", "Service", "NetworkPolicy"}
	if !request.KeepData {
		kinds = append(kinds, "PersistentVolumeClaim", "Secret")
	}
	for _, kind := range kinds {
		ri := m.resource(kind)
		object, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !owned(object, labels) {
			return nil, status.Errorf(codes.PermissionDenied, "%s has another owner", kind)
		}
		uid := object.GetUID()
		if err := ri.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, PropagationPolicy: new(metav1.DeletePropagationForeground)}); err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		// Wait for compute to stop before releasing its disk/credentials.
		if err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
			_, err := ri.Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}); err != nil {
			return nil, err
		}
	}
	return &addonv1.DeprovisionResponse{}, nil
}

var resources = map[string]schema.GroupVersionResource{
	"Secret":                {Version: "v1", Resource: "secrets"},
	"Service":               {Version: "v1", Resource: "services"},
	"PersistentVolumeClaim": {Version: "v1", Resource: "persistentvolumeclaims"},
	"Deployment":            {Group: "apps", Version: "v1", Resource: "deployments"},
	"NetworkPolicy":         {Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"},
}

func (m *mariaDB) resource(kind string) dynamic.ResourceInterface {
	return m.client.Resource(resources[kind]).Namespace(m.namespace)
}

func owned(object *unstructured.Unstructured, labels map[string]string) bool {
	for key, value := range labels {
		if object.GetLabels()[key] != value {
			return false
		}
	}
	return true
}

func (m *mariaDB) apply(ctx context.Context, object runtime.Object) error {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: raw}
	ri := m.resource(u.GetKind())
	existing, err := ri.Get(ctx, u.GetName(), metav1.GetOptions{})
	if err == nil {
		if !owned(existing, u.GetLabels()) {
			return fmt.Errorf("refusing to adopt %s with another owner", u.GetKind())
		}
		if existing.GetDeletionTimestamp() != nil {
			return status.Error(codes.Aborted, "instance is still being deleted; retry provisioning")
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	_, err = ri.Patch(ctx, u.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{FieldManager: "donkey-mariadb-plugin", Force: new(true)})
	return err
}
