package config

import (
	"fmt"
	"regexp"
)

// AccessConfig names users and groups assigned a platform or space role.
// Empty membership grants no access.
type AccessConfig struct {
	Users  []string `json:"users,omitempty" yaml:"users,omitempty"`
	Groups []string `json:"groups,omitempty" yaml:"groups,omitempty"`
}

func (a *AccessConfig) Empty() bool {
	return a == nil || (len(a.Users) == 0 && len(a.Groups) == 0)
}

func anyOverlap(wanted, have []string) bool {
	if len(wanted) == 0 || len(have) == 0 {
		return false
	}
	mine := make(map[string]bool, len(have))
	for _, v := range have {
		mine[v] = true
	}
	for _, v := range wanted {
		if mine[v] {
			return true
		}
	}
	return false
}

// accessEntryRE keeps user and group entries to things that can appear in a
// JWT claim comparison: printable ASCII without whitespace.
var accessEntryRE = regexp.MustCompile(`^[\x21-\x7e]+$`)

func (a *AccessConfig) Validate() error {
	if a == nil {
		return nil
	}
	for _, u := range a.Users {
		if !accessEntryRE.MatchString(u) {
			return fmt.Errorf("users entry %q must be an email address or sub:<subject> without spaces", u)
		}
	}
	for _, g := range a.Groups {
		if !accessEntryRE.MatchString(g) {
			return fmt.Errorf("groups entry %q must be a plain group name without spaces", g)
		}
	}
	return nil
}
