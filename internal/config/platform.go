package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/identity"
)

// PlatformConfig is persisted separately from spaces. An optional install-file
// value seeds it; otherwise the first verified sign-in becomes its first admin.
type PlatformConfig struct {
	Admins AccessConfig `json:"admins" yaml:"admins"`
	// UnmaintainedAfterDays is how old an app's running build may get before
	// the console labels it unmaintained. Zero means the default.
	UnmaintainedAfterDays int `json:"unmaintainedAfterDays,omitempty" yaml:"unmaintainedAfterDays,omitempty"`
}

// DefaultUnmaintainedAfterDays is about a quarter: long enough for a quiet,
// finished app, short enough that its base image has not gone stale.
const DefaultUnmaintainedAfterDays = 90

func (p PlatformConfig) Validate() error {
	if p.UnmaintainedAfterDays < 0 || p.UnmaintainedAfterDays > 3650 {
		return fmt.Errorf("unmaintainedAfterDays must be between 1 and 3650 days")
	}
	return (SpaceConfig{Admins: p.Admins}).Validate()
}

// UnmaintainedAfter is the age at which a running build counts as unmaintained.
func (p PlatformConfig) UnmaintainedAfter() time.Duration {
	days := p.UnmaintainedAfterDays
	if days == 0 {
		days = DefaultUnmaintainedAfterDays
	}
	return time.Duration(days) * 24 * time.Hour
}

func (p PlatformConfig) IsAdmin(id *identity.Identity) bool {
	return managementMember(p.Admins, id)
}

func FirstPlatformAdmin(id *identity.Identity) PlatformConfig {
	user := id.Email
	if user == "" {
		user = "sub:" + id.Subject
	}
	return PlatformConfig{Admins: AccessConfig{Users: []string{user}}}
}

// Management users are email addresses, or sub:<subject> for IdPs that do not
// issue email claims. Subject IDs are exact and never compared as email.
func managementMember(members AccessConfig, id *identity.Identity) bool {
	if id == nil {
		return false
	}
	for _, user := range members.Users {
		if subject, ok := strings.CutPrefix(user, "sub:"); ok {
			if subject != "" && subject == id.Subject {
				return true
			}
		} else if id.Email != "" && strings.EqualFold(user, id.Email) {
			return true
		}
	}
	return anyOverlap(members.Groups, id.Groups)
}
