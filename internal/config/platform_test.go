package config

import (
	"testing"
	"time"
)

func TestPlatformUnmaintainedAfter(t *testing.T) {
	admins := AccessConfig{Users: []string{"ada@example.com"}}
	if got := (PlatformConfig{Admins: admins}).UnmaintainedAfter(); got != 90*24*time.Hour {
		t.Errorf("default = %v, want 90 days", got)
	}
	if got := (PlatformConfig{Admins: admins, UnmaintainedAfterDays: 180}).UnmaintainedAfter(); got != 180*24*time.Hour {
		t.Errorf("configured = %v, want 180 days", got)
	}
	for _, days := range []int{0, 1, 3650} {
		if err := (PlatformConfig{Admins: admins, UnmaintainedAfterDays: days}).Validate(); err != nil {
			t.Errorf("%d days: %v", days, err)
		}
	}
	for _, days := range []int{-1, 3651} {
		if err := (PlatformConfig{Admins: admins, UnmaintainedAfterDays: days}).Validate(); err == nil {
			t.Errorf("%d days validated", days)
		}
	}
	var settings PlatformConfig
	if err := DecodeYAML([]byte("admins:\n  users: [ada@example.com]\nunmaintainedAfterDays: 30\n"), &settings); err != nil || settings.UnmaintainedAfterDays != 30 {
		t.Errorf("decoded %+v, %v", settings, err)
	}
}
