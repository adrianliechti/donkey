package manifests

import (
	"slices"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// clusterScopedName qualifies a cluster-scoped object with the installation
// that owns it. Two installations on one cluster would otherwise apply the
// same ClusterRoleBinding: server-side apply forces the subject list, so the
// second install would quietly strip the first one's ServiceAccount and take
// its permissions away. Only additional installations need the qualification
// — the default keeps the plain names a single-donkey cluster expects.
func clusterScopedName(base, namespace string) string {
	if namespace == DefaultSystemNamespace {
		return base
	}
	return base + "-" + namespace
}

// serverAccessObjects is what donkey-server is allowed to do.
//
// With one namespace for every app (WorkloadNamespace), everything donkey
// touches is inside two namespaces it was given — so the whole installation
// needs no cluster-scoped object at all: two Roles, two RoleBindings, and a
// cluster that never had to grant donkey anything above a namespace. That is
// also what makes several installations on one cluster safe, since there is
// nothing cluster-wide left for them to collide on.
//
// With a namespace per space, donkey creates and deletes namespaces, which no
// Role can express — so it still binds cluster-admin. Scoping that down is
// only possible by giving up per-space namespaces.
func serverAccessObjects(in SystemInput) []runtime.Object {
	if !in.Shared() {
		return []runtime.Object{&rbacv1.ClusterRoleBinding{
			TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
			ObjectMeta: metav1.ObjectMeta{
				Name:   clusterScopedName(ServerName, in.Namespace),
				Labels: SystemLabels(ServerName),
			},
			RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
			Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: ServerName, Namespace: in.Namespace}},
		}}
	}
	var objs []runtime.Object
	for _, ns := range []string{in.Namespace, in.WorkloadNamespace} {
		rules := serverRoleRules()
		if ns == in.Namespace {
			// Per-app Certificates and DNS records live beside the gateway even
			// when apps share a separate workload namespace.
			rules = append(rules, serverManagedEdgeRoleRules()...)
		}
		objs = append(objs,
			&rbacv1.Role{
				TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
				ObjectMeta: metav1.ObjectMeta{Name: ServerName, Namespace: ns, Labels: SystemLabels(ServerName)},
				Rules:      rules,
			},
			&rbacv1.RoleBinding{
				TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
				ObjectMeta: metav1.ObjectMeta{Name: ServerName, Namespace: ns, Labels: SystemLabels(ServerName)},
				RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: ServerName},
				Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: ServerName, Namespace: in.Namespace}},
			},
		)
	}
	return objs
}

// orchestratorAccessObjects is what donkey-orchestrator is allowed to do,
// under a ServiceAccount of its own: what a deploy needs, and none of what
// only the API does.
//
// With a namespace per space a deploy into a new space creates its
// namespace, which no Role can express — the same reason donkey-server binds
// cluster-admin in that arrangement, so the orchestrator does too. With one
// workload namespace it gets namespaced Roles like the server's, minus the
// file browser's pods/exec and the Metrics API reads of resource telemetry.
func orchestratorAccessObjects(in SystemInput) []runtime.Object {
	if !in.Shared() {
		return []runtime.Object{&rbacv1.ClusterRoleBinding{
			TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
			ObjectMeta: metav1.ObjectMeta{
				Name:   clusterScopedName(OrchestratorName, in.Namespace),
				Labels: SystemLabels(OrchestratorName),
			},
			RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
			Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: OrchestratorName, Namespace: in.Namespace}},
		}}
	}
	var objs []runtime.Object
	for _, ns := range []string{in.Namespace, in.WorkloadNamespace} {
		rules := orchestratorRoleRules()
		if ns == in.Namespace {
			// A deploy applies the app's exact-host Certificate and DNS record,
			// which live beside the gateway.
			rules = append(rules, serverManagedEdgeRoleRules()...)
		}
		objs = append(objs,
			&rbacv1.Role{
				TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
				ObjectMeta: metav1.ObjectMeta{Name: OrchestratorName, Namespace: ns, Labels: SystemLabels(OrchestratorName)},
				Rules:      rules,
			},
			&rbacv1.RoleBinding{
				TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
				ObjectMeta: metav1.ObjectMeta{Name: OrchestratorName, Namespace: ns, Labels: SystemLabels(OrchestratorName)},
				RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: OrchestratorName},
				Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: OrchestratorName, Namespace: in.Namespace}},
			},
		)
	}
	return objs
}

// orchestratorRoleRules is serverRoleRules without what only the API does:
// pods/exec is the file browser's way onto an app's disk, and the Metrics
// API feeds resource telemetry, which the server samples. Deploys remove a
// file-browser helper before they roll (plain pod deletion), and they never
// exec into one.
func orchestratorRoleRules() []rbacv1.PolicyRule {
	var rules []rbacv1.PolicyRule
	for _, rule := range serverRoleRules() {
		if slices.Contains(rule.Resources, "pods/exec") || slices.Contains(rule.APIGroups, "metrics.k8s.io") {
			continue
		}
		rules = append(rules, rule)
	}
	return rules
}

// serverManagedEdgeRoleRules is the small additional surface needed to apply
// and retract exact-host Certificates and optional DNS records.
func serverManagedEdgeRoleRules() []rbacv1.PolicyRule {
	// list/deletecollection are used only with the installation + space + edge
	// selector when a whole space is removed.
	writeNamed := []string{"get", "list", "create", "update", "patch", "delete", "deletecollection"}
	return []rbacv1.PolicyRule{
		{
			APIGroups: []string{"cert-manager.io"},
			Resources: []string{"certificates"},
			Verbs:     writeNamed,
		},
		{
			APIGroups: []string{"externaldns.k8s.io"},
			Resources: []string{"dnsendpoints"},
			Verbs:     writeNamed,
		},
	}
}

// serverRoleRules is everything donkey-server does inside a namespace. It is
// deliberately spelled out rather than wildcarded: the point of this mode is
// that an administrator can read the Role and see the whole blast radius.
//
// pods/exec is the one that looks alarming and isn't optional — it is how the
// file browser reads and writes an app's disk, through a short-lived helper
// pod that mounts the volume.
func serverRoleRules() []rbacv1.PolicyRule {
	write := []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection"}
	return []rbacv1.PolicyRule{
		{
			APIGroups: []string{""},
			Resources: []string{"configmaps", "secrets", "services", "persistentvolumeclaims", "pods"},
			Verbs:     write,
		},
		{
			APIGroups: []string{""},
			Resources: []string{"pods/log"},
			Verbs:     []string{"get", "list"},
		},
		{
			// Resource telemetry reads the standard Metrics API. In most
			// clusters Metrics Server provides this aggregated API.
			APIGroups: []string{"metrics.k8s.io"},
			Resources: []string{"pods"},
			Verbs:     []string{"get", "list"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{"pods/exec"},
			Verbs:     []string{"get", "create"},
		},
		{
			// Rollout diagnosis: what the kubelet and the scheduler said about
			// a pod that will not start.
			APIGroups: []string{""},
			Resources: []string{"events"},
			Verbs:     []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{"apps"},
			Resources: []string{"deployments", "replicasets"},
			Verbs:     write,
		},
		{
			// Per-space isolation fences (kubernetes.isolateSpaces). Retained
			// when the feature is off so a later deploy can retract them.
			APIGroups: []string{"networking.k8s.io"},
			Resources: []string{"networkpolicies"},
			Verbs:     write,
		},
	}
}

// donkeyGatewayRouteAccess lets the gateway read the route ConfigMaps its
// routing table is built from — cluster-wide when spaces have namespaces of
// their own, and otherwise only in the two namespaces this installation was
// given, which is what keeps a second installation's routes out of this
// gateway's table.
func donkeyGatewayRouteAccess(in SystemInput) []runtime.Object {
	rules := []rbacv1.PolicyRule{{
		APIGroups: []string{""},
		Resources: []string{"configmaps"},
		Verbs:     []string{"get", "list", "watch"},
	}}
	if !in.Shared() {
		return []runtime.Object{
			&rbacv1.ClusterRole{
				TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
				ObjectMeta: metav1.ObjectMeta{
					Name:   clusterScopedName(GatewayAppName, in.Namespace),
					Labels: SystemLabels(GatewayAppName),
				},
				Rules: rules,
			},
			&rbacv1.ClusterRoleBinding{
				TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
				ObjectMeta: metav1.ObjectMeta{
					Name:   clusterScopedName(GatewayAppName, in.Namespace),
					Labels: SystemLabels(GatewayAppName),
				},
				RoleRef: rbacv1.RoleRef{
					APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole",
					Name: clusterScopedName(GatewayAppName, in.Namespace),
				},
				Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: GatewayAppName, Namespace: in.Namespace}},
			},
		}
	}
	var objs []runtime.Object
	for _, ns := range []string{in.Namespace, in.WorkloadNamespace} {
		name := GatewayAppName + "-routes"
		objs = append(objs,
			&rbacv1.Role{
				TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: SystemLabels(GatewayAppName)},
				Rules:      rules,
			},
			&rbacv1.RoleBinding{
				TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: SystemLabels(GatewayAppName)},
				RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: name},
				Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: GatewayAppName, Namespace: in.Namespace}},
			},
		)
	}
	return objs
}

// donkeyGatewaySecretsRole lets the gateway keep its certificates: the cache
// of issued certificates, the ACME account and the built-in CA all live in
// Secrets of donkey-system — and only there.
func donkeyGatewaySecretsRole(in SystemInput) *rbacv1.Role {
	return &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: GatewayAppName, Namespace: in.Namespace, Labels: SystemLabels(GatewayAppName)},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"secrets"},
			Verbs:     []string{"get", "list", "create", "update"},
		}},
	}
}

func donkeyGatewaySecretsRoleBinding(in SystemInput) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: GatewayAppName, Namespace: in.Namespace, Labels: SystemLabels(GatewayAppName)},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: GatewayAppName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: GatewayAppName, Namespace: in.Namespace}},
	}
}
