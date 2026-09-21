// This standalone gRPC service imports only Donkey's public addon API. It
// owns MariaDB instances in its own namespace, outside app namespaces.
package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	config, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return err
	}
	namespace, installation := os.Getenv("PLUGIN_NAMESPACE"), os.Getenv("DONKEY_INSTALLATION")
	if namespace == "" || installation == "" {
		return fmt.Errorf("PLUGIN_NAMESPACE and DONKEY_INSTALLATION are required")
	}
	address := os.Getenv("LISTEN_ADDR")
	if address == "" {
		address = ":50051"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	var options []grpc.ServerOption
	if cert, key := os.Getenv("TLS_CERT_FILE"), os.Getenv("TLS_KEY_FILE"); cert != "" || key != "" {
		creds, err := credentials.NewServerTLSFromFile(cert, key)
		if err != nil {
			return err
		}
		options = append(options, grpc.Creds(creds))
	}
	token, err := bearerToken()
	if err != nil {
		return err
	}
	options = append(options, authOptions(token)...)
	server := grpc.NewServer(options...)
	addonv1.RegisterServiceProviderServer(server, &mariaDB{client: client, namespace: namespace, installation: installation})
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthv1.HealthCheckResponse_SERVING)
	healthv1.RegisterHealthServer(server, healthServer)
	go func() {
		<-ctx.Done()
		healthServer.Shutdown()
		timer := time.AfterFunc(10*time.Second, server.Stop)
		defer timer.Stop()
		server.GracefulStop()
	}()
	log.Printf("MariaDB service provider listening on %s; database namespace %s", address, namespace)
	return server.Serve(listener)
}

// bearerToken is the shared secret Donkey must present (its addonPlugins
// entry's token or tokenFile). Empty means every caller the network admits.
func bearerToken() (string, error) {
	if file := os.Getenv("PLUGIN_TOKEN_FILE"); file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}
	return os.Getenv("PLUGIN_TOKEN"), nil
}

// authOptions rejects addon calls without the bearer token. Health checks
// stay open: the readiness probe carries no credentials.
func authOptions(token string) []grpc.ServerOption {
	if token == "" {
		return nil
	}
	check := func(ctx context.Context, method string) error {
		if strings.HasPrefix(method, "/grpc.health.v1.Health/") {
			return nil
		}
		md, _ := metadata.FromIncomingContext(ctx)
		for _, value := range md.Get("authorization") {
			if subtle.ConstantTimeCompare([]byte(value), []byte("Bearer "+token)) == 1 {
				return nil
			}
		}
		return status.Error(codes.Unauthenticated, "a bearer token is required")
	}
	return []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			if err := check(ctx, info.FullMethod); err != nil {
				return nil, err
			}
			return handler(ctx, req)
		}),
		grpc.ChainStreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			if err := check(ss.Context(), info.FullMethod); err != nil {
				return err
			}
			return handler(srv, ss)
		}),
	}
}
