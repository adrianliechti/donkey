package servicepolicy

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

func TestExampleServicePolicy(t *testing.T) {
	module, err := os.ReadFile("../../examples/service-policy.rego")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rules.rego"), module, 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "server.yaml")
	if err := os.WriteFile(file, []byte("servicePolicy:\n  regoFile: rules.rego\n  offerings:\n    archive: {tags: [write], authorization: delegated}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	settings, err := config.LoadInstallFile(file)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compile(settings.ServicePolicy, nil)
	if err != nil {
		t.Fatal(err)
	}
	archive := p.Offering(&addonv1.Offering{Id: "archive", Tags: []string{"c4", "write"}, Protocol: "http", Category: "api"})
	if !slices.Equal(archive.Tags, []string{"c4", "write"}) || archive.Protocol != "http" || archive.Category != "api" {
		t.Fatal(archive)
	}
	archive.Name = "vault"
	classes := []string{"C1", "C2", "C3", "C4"}
	all := []Service{archive}
	for _, level := range classes {
		all = append(all, Service{ID: "source", Name: "read-" + level, Tags: []string{"read", level}})
	}
	if reason, err := p.Decision(t.Context(), "team", all); reason != "" || err != nil {
		t.Fatalf("read all classes and write C4: %s %v", reason, err)
	}
	for i, read := range classes {
		for j, write := range classes {
			src := Service{ID: "source", Name: "input", Tags: []string{"read", read}}
			dst := Service{ID: "sink", Name: "output", Tags: []string{"write", write}}
			for _, services := range [][]Service{{src, dst}, {dst, src}} {
				reason, err := p.Decision(t.Context(), "team", services)
				if err != nil || (reason != "") != (i > j) {
					t.Fatalf("read %s/write %s: %q %v", read, write, reason, err)
				}
			}
		}
	}
	src := Service{ID: "case-files", Name: "renamed", Tags: []string{"read", "C4"}}
	for _, mode := range []string{"delegated", "shared", "unknown"} {
		dst := archive
		dst.Authorization = mode
		reason, err := p.Decision(t.Context(), "team", []Service{src, dst})
		if err != nil || strings.Contains(reason, "requires delegated") != (mode != "delegated") {
			t.Fatalf("authorization %s: %q %v", mode, reason, err)
		}
	}
	for _, tags := range [][]string{nil, {"C2"}, {"read", "C1", "C3"}} {
		reason, err := p.Decision(t.Context(), "team", []Service{{Name: "untagged", Tags: tags}})
		if err != nil || reason == "" {
			t.Fatalf("bad tags %v: %q %v", tags, reason, err)
		}
	}
}

func TestInvalidServicePolicyFailsClosed(t *testing.T) {
	for _, module := range []string{"", "not rego", "package other\ndeny := []", "package donkey.services\nallow := true"} {
		if _, err := Compile(&config.ServicePolicy{Rego: module}, nil); err == nil {
			t.Fatalf("compiled invalid policy %q", module)
		}
	}
	for _, rule := range []string{"deny := false", "deny := [true]", "deny := [] if input.space == \"elsewhere\"", "deny := [1/0]"} {
		p, err := Compile(&config.ServicePolicy{Rego: "package donkey.services\n" + rule}, nil)
		if err == nil {
			if _, err := p.Decision(t.Context(), "team", []Service{}); err == nil {
				t.Fatalf("accepted invalid decision %q", rule)
			}
		}
	}
}

func TestSampleProxyMetadata(t *testing.T) {
	settings, err := config.LoadInstallFile("../../examples/service-policy/server.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := settings.ProxyServices.Validate(); err != nil {
		t.Fatal(err)
	}
	p, err := Compile(settings.ServicePolicy, settings.ProxyServices)
	if err != nil {
		t.Fatal(err)
	}
	var selected []Service
	for _, proxy := range settings.ProxyServices {
		service := p.Offering(&addonv1.Offering{Id: proxy.Name, Tags: proxy.Tags})
		service.Name = proxy.Name
		if service.Authorization != proxy.Authorization {
			t.Fatalf("%s authorization = %s", service.Name, service.Authorization)
		}
		if proxy.Name != "reporting" {
			selected = append(selected, service)
		}
	}
	if denied, err := p.Decision(t.Context(), "test", selected); err != nil || denied != "" {
		t.Fatalf("sample: %q, %v", denied, err)
	}
	settings.ServicePolicy.Offerings["case-files"] = config.OfferingPolicyMetadata{Authorization: "shared"}
	if _, err := Compile(settings.ServicePolicy, settings.ProxyServices); err == nil {
		t.Fatal("accepted conflicting proxy authorization")
	}
	proxy := config.ProxyService{Name: "proxy", TargetURL: "https://shared@example.org", OfferingPolicyMetadata: config.OfferingPolicyMetadata{Authorization: "delegated"}}
	if err := (config.ProxyServices{proxy}).Validate(); err == nil {
		t.Fatal("accepted delegated authorization with fixed target credentials")
	}
}
