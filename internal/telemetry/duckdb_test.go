package telemetry

// Store tests run in their own binary, so register the same native driver as
// the donkey-server telemetry role without coupling telemetry's shared
// client/model types to CGO in donkey CLI builds.
import _ "github.com/duckdb/duckdb-go/v2"
