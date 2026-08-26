package server

import (
	"reflect"
	"testing"
)

func TestSplitCommaSeparated(t *testing.T) {
	got := splitCommaSeparated("oci-mirror-registry-lgt-com, shared-registry, ,oci-mirror-registry-lgt-com")
	want := []string{"oci-mirror-registry-lgt-com", "shared-registry"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitCommaSeparated() = %v, want %v", got, want)
	}
}
