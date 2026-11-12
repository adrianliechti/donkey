package addons

import (
	"context"
	"strings"
	"testing"
)

func TestRustfsBinding(t *testing.T) {
	a := builtins["s3"]
	in := input
	in.Name = "rustfs"
	got := a.Binding(in)
	// The host is the service's plain name — reachable the way siblings
	// reach each other — and both endpoint keys carry the same address.
	if want := "http://rustfs.donkey-prod.svc:9000"; got["AWS_ENDPOINT_URL_S3"] != want {
		t.Fatalf("AWS_ENDPOINT_URL_S3 = %q, want %q", got["AWS_ENDPOINT_URL_S3"], want)
	}
	if got["S3_URL"] != got["AWS_ENDPOINT_URL_S3"] {
		t.Fatalf("S3_URL = %q, want the endpoint %q", got["S3_URL"], got["AWS_ENDPOINT_URL_S3"])
	}
	if got["AWS_SECRET_ACCESS_KEY"] != "s3cret" {
		t.Fatalf("AWS_SECRET_ACCESS_KEY = %q, want the provisioning password", got["AWS_SECRET_ACCESS_KEY"])
	}
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_REGION"} {
		if got[k] == "" {
			t.Fatalf("%s missing from binding", k)
		}
	}
}

func TestRustfsExternalBinding(t *testing.T) {
	a := builtins["s3"]
	got, err := a.ExternalBinding("https://s3.example.com/")
	if err != nil {
		t.Fatal(err)
	}
	// The trailing slash has to go: SDKs join bucket paths onto it.
	if got["AWS_ENDPOINT_URL_S3"] != "https://s3.example.com" {
		t.Fatalf("AWS_ENDPOINT_URL_S3 = %q", got["AWS_ENDPOINT_URL_S3"])
	}
	if got["S3_URL"] != "https://s3.example.com" {
		t.Fatalf("S3_URL = %q", got["S3_URL"])
	}
	// A bare URL carries no credentials, so the external binding must not
	// invent any — the app keeps the keys it already has.
	for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if _, ok := got[k]; ok {
			t.Fatalf("external binding must not fabricate %s", k)
		}
	}
	for _, bad := range []string{"s3.example.com", "postgres://u:p@h/db", "http://", "ftp://s3.example.com"} {
		if _, err := a.ExternalBinding(bad); err == nil {
			t.Fatalf("%q should not pass as an S3 endpoint", bad)
		}
	}
}

func TestGetListsS3(t *testing.T) {
	_, err := Get(context.Background(), "miniofs")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "s3") {
		t.Fatalf("error should offer s3: %v", err)
	}
}
