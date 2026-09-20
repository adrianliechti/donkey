// This standalone gRPC service imports only Donkey's public protobuf API.
// It owns MariaDB instances in its own namespace, outside app namespaces.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
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
	server := grpc.NewServer(options...)
	addonv1.RegisterAddonPluginServer(server, &mariaDB{client: client, namespace: namespace, installation: installation})
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
	log.Printf("MariaDB addon plugin listening on %s; database namespace %s", address, namespace)
	return server.Serve(listener)
}
