package config

import (
	"strings"

	"github.com/adrianliechti/donkey/internal/identity"
)

// PlatformConfig is persisted separately from spaces. An optional install-file
// value seeds it; otherwise the first verified sign-in becomes its first admin.
type PlatformConfig struct {
	Admins AccessConfig `json:"admins" yaml:"admins"`
}

func (p PlatformConfig) Validate() error {
	return (SpaceConfig{Admins: p.Admins}).Validate()
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
	return anyOverlap(members.Groups, id.Groups) || anyOverlap(members.Roles, id.Roles)
}
