package addons

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	pgUser = "donkey"
	pgDB   = "app"
	pgPort = 5432
)

type postgres struct{}

func (postgres) Type() string { return "postgres" }
func (postgres) Description() string {
	return "The relational database — most frameworks speak to it out of the box."
}
func (postgres) CredentialKey() string  { return "PGPASSWORD" }
func (postgres) DefaultVersion() string { return "18.6" }
func (postgres) DefaultStorage() string { return "1Gi" }

func (postgres) Binding(in Input) map[string]string {
	// The service is named plainly after itself, so siblings in the space
	// reach it as "postgres" — the same way they reach each other.
	host := fmt.Sprintf("%s.%s.svc", in.HostName(), in.Namespace)
	return map[string]string{
		"DATABASE_URL": fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
			pgUser, in.Password, host, pgPort, pgDB),
		// JDBC drivers reject postgres:// URLs, so the Java world gets the
		// same connection under Heroku's JDBC_* convention.
		"JDBC_DATABASE_URL": fmt.Sprintf("jdbc:postgresql://%s:%d/%s?user=%s&password=%s&sslmode=disable",
			host, pgPort, pgDB, pgUser, in.Password),
		"JDBC_DATABASE_USERNAME": pgUser,
		"JDBC_DATABASE_PASSWORD": in.Password,
		"PGHOST":                 host,
		"PGPORT":                 fmt.Sprintf("%d", pgPort),
		"PGUSER":                 pgUser,
		"PGPASSWORD":             in.Password,
		"PGDATABASE":             pgDB,
	}
}

func (p postgres) Workload(in Input) (*Workload, error) {
	return &Workload{
		Image:    in.ImageRef("postgres"),
		Port:     pgPort,
		PortName: "postgres",
		Env: map[string]string{
			"POSTGRES_USER": pgUser,
			"POSTGRES_DB":   pgDB,
			"PGDATA":        "/var/lib/postgresql/data/pgdata",
		},
		SecretEnv: map[string]string{"POSTGRES_PASSWORD": "PGPASSWORD"},
		DataPath:  "/var/lib/postgresql/data",
		// The image's root entrypoint would chown its way to the postgres
		// user — starting as that user (999) with the volume group-owned
		// skips the whole root phase.
		RunAsUser:    999,
		ReadyCommand: []string{"pg_isready", "-U", pgUser, "-d", pgDB},
	}, nil
}

// ExternalBinding derives the same env contract from a user-supplied
// connection URL, so remote databases bind exactly like provisioned ones.
func (postgres) ExternalBinding(raw string) (map[string]string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return nil, fmt.Errorf("that doesn't look like a postgres URL — expected postgres://user:password@host:5432/dbname")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("the postgres URL names no host — expected postgres://user:password@host:5432/dbname")
	}
	if strings.TrimPrefix(u.Path, "/") == "" {
		return nil, fmt.Errorf("the postgres URL names no database — expected postgres://user:password@host:5432/dbname")
	}
	password, _ := u.User.Password()
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	// JDBC credentials ride as query parameters, where they need escaping the
	// postgres:// userinfo form did not.
	q := u.Query()
	q.Set("user", u.User.Username())
	if password != "" {
		q.Set("password", password)
	}
	return map[string]string{
		"DATABASE_URL":           raw,
		"JDBC_DATABASE_URL":      fmt.Sprintf("jdbc:postgresql://%s:%s%s?%s", u.Hostname(), port, u.Path, q.Encode()),
		"JDBC_DATABASE_USERNAME": u.User.Username(),
		"JDBC_DATABASE_PASSWORD": password,
		"PGHOST":                 u.Hostname(),
		"PGPORT":                 port,
		"PGUSER":                 u.User.Username(),
		"PGPASSWORD":             password,
		"PGDATABASE":             strings.TrimPrefix(u.Path, "/"),
	}, nil
}
