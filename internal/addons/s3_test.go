package addons

import (
	"context"
	"reflect"
	"testing"
)

func TestLegacyS3AliasPreservesBindingAndWorkload(t *testing.T) {
	entry, err := Get(context.Background(), "s3")
	if err != nil {
		t.Fatal(err)
	}
	canonical := entry.(Builtin)
	entry, err = Get(context.Background(), "rustfs")
	if err != nil {
		t.Fatal(err)
	}
	legacy := entry.(Builtin)
	in := input
	in.Name = "rustfs" // An existing service's object names and disk remain unchanged.
	in.Version = "1.0.0-rc.3"
	if !reflect.DeepEqual(legacy.Binding(in), canonical.Binding(in)) {
		t.Fatal("alias changed credentials or endpoints")
	}
	old, err := legacy.Workload(in)
	if err != nil {
		t.Fatal(err)
	}
	current, err := canonical.Workload(in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(old, current) || current.Image != "rustfs/rustfs:1.0.0-rc.3" || current.DataPath != "/data" {
		t.Fatal("rename changed the storage engine or data layout")
	}
	desc, err := legacy.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if desc.Id != "s3" || desc.Title != "S3" || desc.Protocol != ProtocolS3 {
		t.Fatalf("wrong identity: %+v", desc)
	}
	for _, a := range Types(context.Background()) {
		if a.Type() == "rustfs" {
			t.Fatal("legacy alias duplicated the catalog")
		}
	}
	binding := legacy.Binding(in)
	metadata := (Metadata{Type: "rustfs", Protocol: "s3", Parameters: Parameters{ParamStorage: "5Gi"}}).Resolve(binding)
	if metadata.Type != "s3" || metadata.Protocol != "s3" || metadata.Parameters.String(ParamStorage) != "5Gi" || binding["AWS_SECRET_ACCESS_KEY"] != in.Password {
		t.Fatal("metadata normalization changed the binding")
	}
}
