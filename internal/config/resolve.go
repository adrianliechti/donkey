package config

// Flags carries the global CLI flags that participate in config resolution.
type Flags struct {
	KubeContext string
	AppDir      string
	NoAI        bool
	Verbose     bool
}

// Resolved is the single fully-resolved view of client configuration.
// Everything cluster-related (registry, domains, namespaces) lives on the
// server now.
type Resolved struct {
	Flags  Flags
	Global *GlobalConfig // nil when not set up yet
	App    *AppConfig    // nil when no donkey.yaml in the app dir

	// KubeContext is only used by `donkey server install`.
	KubeContext string
}

func Resolve(flags Flags, global *GlobalConfig, app *AppConfig) *Resolved {
	r := &Resolved{Flags: flags, Global: global, App: app}
	if flags.AppDir == "" {
		r.Flags.AppDir = "."
	}
	if global != nil {
		r.KubeContext = global.KubeContext
	}
	if flags.KubeContext != "" {
		r.KubeContext = flags.KubeContext
	}
	return r
}

// ServerURL returns the configured server address ("" when not logged in).
func (r *Resolved) ServerURL() string {
	if r.Global == nil {
		return ""
	}
	return r.Global.Server.URL
}
