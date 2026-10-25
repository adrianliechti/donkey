package manifests

import (
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
)

// Only what was configured travels: a mirrored database and a stricter
// threshold reach the control plane, an unset switch does not.
func TestBuildEnvCarriesScanSettings(t *testing.T) {
	env := buildEnv(config.InstallBuild{BuildSettings: config.BuildSettings{Scan: config.ScanSettings{
		DBRepository: "jfrog.corp/ghcr/aquasecurity/trivy-db", Block: "high",
	}}})
	got := map[string]string{}
	for _, e := range env {
		got[e.Name] = e.Value
	}
	if got["DONKEY_BUILDER_SCAN_DB_REPOSITORY"] != "jfrog.corp/ghcr/aquasecurity/trivy-db" || got["DONKEY_BUILDER_SCAN_BLOCK"] != "high" {
		t.Errorf("scan settings missing from the control plane env: %v", got)
	}
	if _, ok := got["DONKEY_BUILDER_SCAN"]; ok {
		t.Error("an unset switch must not travel")
	}
}
