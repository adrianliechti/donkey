package config

import (
	"strings"
	"testing"
)

func TestProxyServiceValidation(t *testing.T) {
	valid := ProxyService{Name: "enterprise-api", TargetURL: "https://user:password@api.example.com/v1?q=1", ProxyURL: "https://user:password@proxy.example.com:8443"}
	if err := (ProxyServices{valid}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ field, value string }{
		{"name", "Invalid Name"},
		{"target", ""},
		{"target", "ftp://user:password@api.example.com/"},
		{"target", "https://api.example.com/#fragment"},
		{"proxy", "https://user:password@proxy.example.com/path"},
		{"proxy", "https://proxy.example.com/?q=1"},
	} {
		t.Run(tc.field+tc.value, func(t *testing.T) {
			service := valid
			switch tc.field {
			case "name":
				service.Name = tc.value
			case "target":
				service.TargetURL = tc.value
			case "proxy":
				service.ProxyURL = tc.value
			}
			err := (ProxyServices{service}).Validate()
			if err == nil || strings.Contains(err.Error(), "password") {
				t.Errorf("invalid configuration accepted or leaked a credential: %v", err)
			}
		})
	}
	if err := (ProxyServices{valid, valid}).Validate(); err == nil {
		t.Fatal("duplicate proxy offerings were accepted")
	}
}
