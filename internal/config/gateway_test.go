package config

import "testing"

func TestExternalDNSRecordTypeInference(t *testing.T) {
	for _, tc := range []struct {
		name    string
		targets []string
		want    string
	}{
		{name: "disabled", want: ""},
		{name: "IPv4 VIP", targets: []string{"192.0.2.10"}, want: "A"},
		{name: "IPv6 VIP", targets: []string{"2001:db8::5"}, want: "AAAA"},
		{name: "load balancer hostname", targets: []string{"gateway.example.com"}, want: "CNAME"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (InstallExternalDNS{Targets: tc.targets}).EffectiveRecordType(); got != tc.want {
				t.Errorf("record type = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExternalDNSExplicitRecordTypeIsNormalized(t *testing.T) {
	dns := InstallExternalDNS{Targets: []string{"192.0.2.10"}, RecordType: "a"}
	if err := dns.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := dns.EffectiveRecordType(); got != "A" {
		t.Fatalf("record type = %q, want A", got)
	}
}
