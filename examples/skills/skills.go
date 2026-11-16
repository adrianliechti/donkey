// Package skills holds the donkey skill for coding agents: skill.md, the file
// to copy into an agent's skills folder so it knows how to deploy and operate
// apps with the donkey CLI. A personal machine installs it for its agent, so
// the copy an agent reads there is always the one its CLI was built with.
package skills

import _ "embed"

// Donkey is the skill, as an agent's skills folder takes it (SKILL.md).
//
//go:embed skill.md
var Donkey string
