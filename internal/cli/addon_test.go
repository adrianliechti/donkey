package cli

import "testing"

func TestTunnelBindingPreservesCredentialsAndOtherDestinations(t *testing.T) {
	for _, tc := range []struct{ key, value, host, want string }{
		{"MYSQL_PASSWORD", "db:3306", "db", "db:3306"},
		{"MYSQL_PASSWORD", "3306", "db", "3306"},
		{"MYSQL_USER", "db", "db", "db"},
		{"MYSQL_HOST", "db", "db", "localhost"},
		{"MYSQL_PORT", "3306", "db", "13306"},
		{"MYSQL_PORT", "5432", "db", "5432"},
		{"DATABASE_URL", "mysql://db:db@db:3306/db?label=db:3306", "db", "mysql://db:db@localhost:13306/db?label=db:3306"},
		{"DATABASE_URL", "mysql://u:p%40ss@[2001:db8::1]:3306/app", "2001:db8::1", "mysql://u:p%40ss@localhost:13306/app"},
		{"DATABASE_URL", "mysql://db:db@other:3306/db", "db", "mysql://db:db@other:3306/db"},
		{"DATABASE_URL", "mysql://u:p@db:3307/app", "db", "mysql://u:p@db:3307/app"},
		{"JDBC_DATABASE_URL", "jdbc:mysql://db/app", "db", "jdbc:mysql://localhost:13306/app"},
		{"REDIS_ADDR", "[::1]:3306", "::1", "localhost:13306"},
	} {
		if got := localizeBinding(tc.key, tc.value, tc.host, 3306, 13306); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.key, got, tc.want)
		}
	}
}
