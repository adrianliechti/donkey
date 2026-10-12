package addons

import (
	"context"
	"reflect"
	"testing"
)

func TestExaBinding(t *testing.T) {
	entry, err := Get(context.Background(), "exa")
	if err != nil {
		t.Fatal(err)
	}
	a := entry.(Builtin)
	desc, err := a.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if desc.Protocol != ProtocolExa || desc.Capabilities != nil {
		t.Fatalf("wrong descriptor: %+v", desc)
	}
	in := Input{ExaProxyURL: "http://donkey-ai:8089/exa", Password: "space-token"}
	got := a.Binding(in)
	if got["EXA_BASE_URL"] != in.ExaProxyURL || got["EXA_API_KEY"] != in.Password {
		t.Fatalf("binding: %v", got)
	}
	if err := ValidateBinding(desc.Protocol, got); err != nil {
		t.Fatal(err)
	}
	if w, err := a.Workload(in); err != nil || w != nil {
		t.Fatalf("binding-only addon: %v %v", w, err)
	}
	for _, raw := range []string{"https://api.exa.ai/", "http://localhost:4242/exa/"} {
		binding, err := a.ExternalBinding(raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(binding) != 1 || binding["EXA_API_KEY"] != "" {
			t.Fatalf("external key must belong to the app: %v", binding)
		}
	}
	for _, raw := range []string{"api.exa.ai", "ftp://api.exa.ai", "http:///exa", "https://u:secret@api.exa.ai", "https://api.exa.ai?key=secret", "https://api.exa.ai#contents"} {
		if _, err := a.ExternalBinding(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	if !reflect.DeepEqual(EnvNames(desc), []string{"EXA_API_KEY", "EXA_BASE_URL"}) {
		t.Fatalf("env contract: %v", EnvNames(desc))
	}
}
