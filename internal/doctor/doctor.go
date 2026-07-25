// Package doctor is donkey's client-side environment check engine, backing
// `donkey doctor` (the server contributes its own checks via the API).
package doctor

import (
	"context"
	"time"

	"monobox.ch/donkey/internal/config"
	"monobox.ch/donkey/internal/ui"
)

type Status int

const (
	OK Status = iota
	Info
	Warn
	Fail
)

type Result struct {
	Status Status
	// Detail is a short plain-language elaboration shown next to the check name.
	Detail string
	// Err is the underlying technical error, shown only in verbose mode.
	Err error
}

type Check struct {
	ID   string
	Name string // friendly, e.g. "Connected to the donkey server"
	// Explain says why donkey needs this, in ordinary words.
	Explain string
	// Fix tells the user what to do when the check fails.
	Fix string
	Run func(ctx context.Context, env *Env) Result
}

// Env is everything checks may inspect. Fields are optional; checks must
// degrade gracefully when something is nil.
type Env struct {
	Global     *config.GlobalConfig
	GlobalPath string
	AppDir     string
	App        *config.AppConfig
}

func NewEnv(global *config.GlobalConfig, globalPath, appDir string, app *config.AppConfig) *Env {
	return &Env{Global: global, GlobalPath: globalPath, AppDir: appDir, App: app}
}

const checkTimeout = 30 * time.Second

// Report runs every check and prints the doctor report. Returns true when
// nothing failed.
func Report(ctx context.Context, u *ui.UI, env *Env, checks []*Check) bool {
	ok := true
	for _, c := range checks {
		cctx, cancel := context.WithTimeout(ctx, checkTimeout)
		r := c.Run(cctx, env)
		cancel()
		switch r.Status {
		case OK:
			u.Success("%s%s", c.Name, suffix(r.Detail))
		case Info:
			u.Info("%s%s", c.Name, suffix(r.Detail))
		case Warn:
			u.Warn("%s%s", c.Name, suffix(r.Detail))
		case Fail:
			ok = false
			u.Fail("%s — %s", c.Name, r.Detail)
			if c.Explain != "" {
				u.Printf("  %s", u.Faint(c.Explain))
			}
			if c.Fix != "" {
				u.Printf("  %s", u.Faint("How to fix: "+c.Fix))
			}
			if r.Err != nil {
				u.Detail("%v", r.Err)
			}
		}
	}
	return ok
}

func suffix(detail string) string {
	if detail == "" {
		return ""
	}
	return " — " + detail
}
