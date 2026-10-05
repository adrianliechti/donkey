package config

import "testing"

func TestAddonPluginConfiguration(t *testing.T) {
	for _, plugins := range []AddonPlugins{
		{{Name: "mariadb", Address: "db.plugins.svc:50051", Insecure: true}},
		{{Name: "remote", Address: "[2001:db8::1]:443"}},
	} {
		if err := plugins.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, plugins := range []AddonPlugins{
		{{Name: "Bad Name", Address: "db:50051"}},
		{{Name: "db", Address: "db:50051"}, {Name: "db", Address: "other:50051"}},
		{{Name: "db", Address: "db"}},
		{{Name: "db", Address: ":50051"}},
		{{Name: "db", Address: "db:0"}},
		{{Name: "db", Address: "db:70000"}},
	} {
		if err := plugins.Validate(); err == nil {
			t.Fatalf("accepted invalid configuration: %+v", plugins)
		}
	}
}
