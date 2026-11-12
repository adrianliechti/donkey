// Package addonv1 is the public, versioned gRPC contract for addon plugins.
package addonv1

//go:generate protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative addon.proto
