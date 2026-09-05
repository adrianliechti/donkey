package manifests

import (
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	appsv1 "k8s.io/api/apps/v1"
)

func TestDefaultWorkloadsHaveRequestsWithoutResourceLimits(t *testing.T) {
	for _, object := range SystemObjects(systemInput()) {
		deployment, ok := object.(*appsv1.Deployment)
		if !ok {
			continue
		}
		for _, container := range deployment.Spec.Template.Spec.Containers {
			if len(container.Resources.Limits) != 0 {
				t.Errorf("%s/%s has default resource limits: %v", deployment.Name, container.Name, container.Resources.Limits)
			}
		}
	}
	app := &config.AppConfig{}
	app.ApplyDefaults()
	if len(app.Resources.Requests) == 0 || len(app.Resources.Limits) != 0 {
		t.Fatalf("app defaults should reserve capacity without a ceiling: %+v", app.Resources)
	}
}
