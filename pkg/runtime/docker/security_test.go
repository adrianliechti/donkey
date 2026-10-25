package docker

import (
	"context"
	"reflect"
	"testing"
)

func TestUpdateSecurityOnlyChangesStoredMetadata(t *testing.T) {
	r := &Runtime{state: state{dir: t.TempDir()}}
	original := runSpec{Image: "registry/app:1", Port: 8080, Command: []string{"server"}, Env: map[string]string{"MODE": "production"}, Security: "old"}
	if err := r.setRunRecord("team", "app", original); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateSecurity(context.Background(), "team", "app", "registry/app:2", "wrong"); err == nil {
		t.Fatal("updated a different image")
	}
	if err := r.UpdateSecurity(context.Background(), "team", "app", "registry/app:1", "new"); err != nil {
		t.Fatal(err)
	}
	want := original
	want.Security = "new"
	got, err := r.runRecord("team", "app")
	if err != nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("workload changed: %+v %v", got, err)
	}
	// No Docker client is present: touching containers would panic here.
}
