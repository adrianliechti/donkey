package config

import (
	"strings"
	"testing"
)

func TestAppGatewayNameDefaultsToDeploymentName(t *testing.T) {
	got, err := AppGatewayName("", "donkey-system", "acme", "shop", "shop")
	if err != nil {
		t.Fatal(err)
	}
	if got != "shop" {
		t.Fatalf("default app Gateway name = %q, want Deployment name shop", got)
	}
}

func TestAppGatewayNameExpandsParameters(t *testing.T) {
	const template = "$namespace-${space}-$app-$name-$hash"
	first, err := AppGatewayName(template, "donkey-system", "acme", "shop", "shop")
	if err != nil {
		t.Fatal(err)
	}
	second, err := AppGatewayName(template, "donkey-system", "acme", "shop", "shop")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("stable template rendered %q then %q", first, second)
	}
	if !strings.HasPrefix(first, "donkey-system-acme-shop-shop-") || len(strings.TrimPrefix(first, "donkey-system-acme-shop-shop-")) != 10 {
		t.Fatalf("template parameters rendered unexpectedly: %q", first)
	}

	other, err := AppGatewayName(template, "donkey-system", "other", "shop", "shop")
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatal("$hash should distinguish the same app name in different spaces")
	}
}

func TestAppGatewayNameRejectsInvalidTemplates(t *testing.T) {
	for _, template := range []string{"$unknown", "UPPER-$name", "$name-"} {
		if _, err := AppGatewayName(template, "donkey-system", "acme", "shop", "shop"); err == nil {
			t.Errorf("AppGatewayName(%q) should fail", template)
		}
	}
}
