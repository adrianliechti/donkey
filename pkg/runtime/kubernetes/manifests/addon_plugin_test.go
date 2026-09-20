package manifests

import (
	"reflect"
	"testing"

	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestPluginEgressPinsNamespacePodsAndTranslatedPort(t *testing.T) {
	info := &addonv1.ProvisionResponse{Env: map[string]string{"MYSQL_HOST": "db.addons.svc"}, Endpoints: []*addonv1.Endpoint{
		{Host: "db.addons.svc", Port: 3306, Kubernetes: &addonv1.KubernetesTarget{Namespace: "addons", PodLabels: map[string]string{"db": "team"}, Port: 13306}},
		{Host: "backup.example.com", Port: 443, Cidrs: []string{"192.0.2.10/32", "2001:db8::10/128"}},
	}}
	p, err := AddonEgressPolicy("team", "workloads", "main", "db", "team-db", info)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "team-db-egress" || !reflect.DeepEqual(p.Spec.PodSelector.MatchLabels, SpacePodSelector("main", "team")) {
		t.Fatal("policy selects another space or installation")
	}
	if len(p.Spec.Ingress) != 0 || len(p.Spec.Egress) != 2 {
		t.Fatal("unexpected policy rules")
	}
	rule := p.Spec.Egress[0]
	if len(rule.To) != 1 || rule.To[0].NamespaceSelector.MatchLabels[LabelNamespaceName] != "addons" || rule.To[0].PodSelector.MatchLabels["db"] != "team" {
		t.Fatal("pod and namespace restrictions must intersect in a single peer")
	}
	if len(rule.Ports) != 1 || *rule.Ports[0].Protocol != corev1.ProtocolTCP || rule.Ports[0].Port.IntVal != 13306 {
		t.Fatal("did not use the translated pod port")
	}
	if peers := p.Spec.Egress[1].To; len(peers) != 2 || peers[0].IPBlock.CIDR != "192.0.2.10/32" || peers[1].IPBlock.CIDR != "2001:db8::10/128" {
		t.Fatal("external CIDRs were widened or lost")
	}
	info.Endpoints[0].Kubernetes.PodLabels["db"] = "changed"
	if rule.To[0].PodSelector.MatchLabels["db"] != "team" {
		t.Fatal("policy aliases the mutable plugin response")
	}
}

func TestPluginEgressRejectsHostnameWithoutPolicyTarget(t *testing.T) {
	info := &addonv1.ProvisionResponse{Env: map[string]string{"MYSQL_HOST": "db.example.com"}, Endpoints: []*addonv1.Endpoint{{Host: "db.example.com", Port: 3306}}}
	if _, err := AddonEgressPolicy("team", "workloads", "main", "db", "team-db", info); err == nil {
		t.Fatal("hostname-only response bypassed restricted egress")
	}
}
