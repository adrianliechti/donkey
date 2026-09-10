package config

// GatewayRoute is the routing configuration shared by Kubernetes ConfigMaps
// and Docker's persisted JSON files.
type GatewayRoute struct {
	Space       string        `json:"space,omitempty" yaml:"space,omitempty"`
	App         string        `json:"app,omitempty" yaml:"app,omitempty"`
	Host        string        `json:"host" yaml:"host"`
	Path        string        `json:"path" yaml:"path"`
	StripPrefix bool          `json:"stripPrefix,omitempty" yaml:"stripPrefix,omitempty"`
	Backend     string        `json:"backend" yaml:"backend"`
	Login       bool          `json:"login,omitempty" yaml:"login,omitempty"`
	Mode        string        `json:"mode,omitempty" yaml:"mode,omitempty"`
	CORS        string        `json:"cors,omitempty" yaml:"cors,omitempty"`
	Access      *AccessConfig `json:"access,omitempty" yaml:"access,omitempty"`
	// Only the platform's own route forwards an identity token to its backend.
	Token bool `json:"token,omitempty" yaml:"token,omitempty"`
}
