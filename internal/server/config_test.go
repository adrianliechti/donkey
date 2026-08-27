package server

import (
	"reflect"
	"testing"
)

func TestSplitCommaSeparated(t *testing.T) {
	got := splitCommaSeparated("oci-mirror-registry-example-com, shared-registry, ,oci-mirror-registry-example-com")
	want := []string{"oci-mirror-registry-example-com", "shared-registry"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitCommaSeparated() = %v, want %v", got, want)
	}
}
