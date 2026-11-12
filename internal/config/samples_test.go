package config

import (
	"path/filepath"
	"testing"
)

// Every sample's donkey.yaml has to be valid — they are the first thing a
// person copies.
func TestSampleConfigsLoad(t *testing.T) {
	matches, err := filepath.Glob("../../testdata/*/donkey.yaml")
	if err != nil {
		t.Fatal(err)
	}
	nested, _ := filepath.Glob("../../testdata/*/*/donkey.yaml")
	deep, _ := filepath.Glob("../../testdata/*/*/*/donkey.yaml")
	demo, _ := filepath.Glob("../../examples/demo/*/donkey.yaml")
	for _, m := range append(append(append(matches, nested...), deep...), demo...) {
		dir := filepath.Dir(m)
		app, err := LoadApp(dir)
		if err != nil {
			t.Errorf("%s: %v", m, err)
			continue
		}
		t.Logf("%-46s internal=%v public=%v routes=%d storage=%v",
			filepath.Base(dir), app.Internal, app.Public, len(app.Routes), app.HasStorage())
	}
}
