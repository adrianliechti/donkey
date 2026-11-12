package addons

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
)

type redis struct{}

func (redis) Type() string          { return "redis" }
func (redis) CredentialKey() string { return "REDIS_PASSWORD" }

func (r redis) Describe(context.Context) (*addonv1.Offering, error) {
	return description(r, "Redis", "A fast key-value store for caches, sessions and queues.",
		CategoryCache, ProtocolRedis, ObjectSchema(map[string]any{
			ParamVersion: VersionParameter("7.4.11", "A tag of the redis image."),
			ParamStorage: StorageParameter("1Gi"),
		}),
		"REDIS_URL", "REDIS_PASSWORD"), nil
}

func (r redis) Binding(in Input) map[string]string {
	u := &url.URL{
		Scheme: "redis",
		Host:   net.JoinHostPort(in.HostName()+"."+in.Namespace+".svc", "6379"),
		User:   url.UserPassword("default", in.Password),
		Path:   "/0",
	}
	binding, _ := r.ExternalBinding(u.String())
	return binding
}

func (redis) ExternalBinding(raw string) (map[string]string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "redis" && u.Scheme != "rediss") || u.Hostname() == "" {
		return nil, fmt.Errorf("expected a Redis URL such as redis://user:password@host:6379/0 (rediss:// for TLS)")
	}
	port := u.Port()
	if port == "" {
		port = "6379"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("the Redis port must be between 1 and 65535")
	}
	db := strings.TrimPrefix(u.Path, "/")
	if db == "" {
		db = "0"
	}
	if n, err := strconv.Atoi(db); err != nil || n < 0 {
		return nil, fmt.Errorf("the Redis database must be a non-negative number, such as /0")
	}
	if u.Fragment != "" {
		return nil, fmt.Errorf("a Redis URL cannot contain a fragment")
	}
	user := u.User.Username()
	if user == "" {
		user = "default"
	}
	password, _ := u.User.Password()
	return map[string]string{
		"REDIS_URL":      raw,
		"REDIS_ADDR":     net.JoinHostPort(u.Hostname(), port),
		"REDIS_HOST":     u.Hostname(),
		"REDIS_PORT":     port,
		"REDIS_USERNAME": user,
		"REDIS_PASSWORD": password,
		"REDIS_DB":       db,
		"REDIS_TLS":      strconv.FormatBool(u.Scheme == "rediss"),
	}, nil
}

func (redis) Workload(in Input) (*Workload, error) {
	return &Workload{
		Image: in.ImageRef("redis"), Port: 6379, PortName: "redis",
		// Keep the image's ownership setup on Docker. Kubernetes already
		// starts as its unprivileged redis user with a group-writable disk.
		Command: []string{"sh", "-c", `exec docker-entrypoint.sh redis-server --appendonly yes --requirepass "$REDIS_PASSWORD"`},
		SecretEnv: map[string]string{
			"REDIS_PASSWORD": "REDIS_PASSWORD", "REDISCLI_AUTH": "REDIS_PASSWORD",
		},
		DataPath: "/data", RunAsUser: 999,
		ReadyCommand: []string{"sh", "-c", `test "$(redis-cli ping)" = PONG`},
	}, nil
}
