package config

// GatewayRoute is the routing configuration shared by Kubernetes ConfigMaps
// and Docker's persisted JSON files.
// Kubernetes takes identity from labels.
type GatewayRoute struct {
	Space       string      `json:"space,omitempty" yaml:"-"`
	App         string      `json:"app,omitempty" yaml:"-"`
	Host        string      `json:"host" yaml:"host"`
	Path        string      `json:"path" yaml:"path"`
	StripPrefix bool        `json:"stripPrefix,omitempty" yaml:"stripPrefix,omitempty"`
	Backend     string      `json:"backend" yaml:"backend"`
	Login       bool        `json:"login,omitempty" yaml:"login,omitempty"`
	Mode        string      `json:"mode,omitempty" yaml:"mode,omitempty"`
	CORS        string      `json:"cors,omitempty" yaml:"cors,omitempty"`
	OIDC        *OIDCClient `json:"oidc,omitempty" yaml:"oidc,omitempty"`
	// Analytics is AnalyticsOff when the app opted out of web analytics;
	// empty leaves the gateway's default (on for web routes).
	Analytics string `json:"analytics,omitempty" yaml:"analytics,omitempty"`
	// Access is read only for migrating old deployed memberships to spaces.
	Access *AccessConfig `json:"access,omitempty" yaml:"-"`
	// Only the platform's own route forwards an identity token to its backend.
	Token bool `json:"token,omitempty" yaml:"token,omitempty"`
}

// AnalyticsOff is the route value for an app that opted out of web analytics.
const AnalyticsOff = "off"

// RouteAnalytics is the route value for an app's analytics choice.
func RouteAnalytics(app *AppConfig) string {
	if app.AnalyticsEnabled() {
		return ""
	}
	return AnalyticsOff
}
