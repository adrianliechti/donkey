package addons

import (
	"context"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/adrianliechti/donkey/internal/config"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type testPluginServer struct {
	addonv1.UnimplementedAddonPluginServer
	typ       string
	protocol  string
	provision func(context.Context, *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error)
	deleted   chan *addonv1.DeprovisionRequest
}

func (s *testPluginServer) Describe(context.Context, *addonv1.DescribeRequest) (*addonv1.DescribeResponse, error) {
	return &addonv1.DescribeResponse{Type: s.typ, Protocol: s.protocol, Title: "Test DB", EnvKeys: []string{"DATABASE_URL", "MYSQL_PASSWORD"}}, nil
}
func (s *testPluginServer) Provision(ctx context.Context, r *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error) {
	return s.provision(ctx, r)
}
func (s *testPluginServer) Deprovision(_ context.Context, r *addonv1.DeprovisionRequest) (*addonv1.DeprovisionResponse, error) {
	s.deleted <- r
	return &addonv1.DeprovisionResponse{}, nil
}

func startPlugin(t *testing.T, server *testPluginServer) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	addonv1.RegisterAddonPluginServer(grpcServer, server)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)
	return listener.Addr().String()
}

func sampleProvisioning() *addonv1.ProvisionResponse {
	return &addonv1.ProvisionResponse{
		Env:       map[string]string{"DATABASE_URL": "mysql://donkey:secret@db.addons.svc:3306/app", "MYSQL_PASSWORD": "secret"},
		Urls:      []string{"mysql://db.addons.svc:3306/app"},
		Endpoints: []*addonv1.Endpoint{{Host: "db.addons.svc", Port: 3306, Kubernetes: &addonv1.KubernetesTarget{Namespace: "addons", PodLabels: map[string]string{"instance": "team-db"}}}},
	}
}

func TestPluginGRPCLifecycleAndEnvironment(t *testing.T) {
	ctx := context.Background()
	instance := &addonv1.Instance{Installation: "main", Space: "team", Name: "db", Runtime: "kubernetes", Namespace: "workloads"}
	want := sampleProvisioning()
	server := &testPluginServer{typ: "testdb", deleted: make(chan *addonv1.DeprovisionRequest, 1), provision: func(ctx context.Context, req *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error) {
		if _, ok := ctx.Deadline(); !ok {
			return nil, status.Error(codes.Internal, "missing deadline")
		}
		if !proto.Equal(instance, req.Instance) || req.Version != "11.8" {
			return nil, status.Error(codes.InvalidArgument, "identity lost")
		}
		return want, nil
	}}
	address := startPlugin(t, server)
	closePlugins, err := ConnectPlugins(ctx, config.AddonPlugins{{Type: server.typ, Address: address, Insecure: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closePlugins)
	a, err := Get(server.typ)
	if err != nil {
		t.Fatal(err)
	}
	if binding := a.Binding(Input{}); len(binding) != 2 || binding["DATABASE_URL"] != "" {
		t.Fatal("catalog must return keys without credentials")
	}
	result, err := a.(*Plugin).Provision(ctx, &addonv1.ProvisionRequest{Instance: instance, Version: "11.8"})
	if err != nil || !proto.Equal(result, want) {
		t.Fatalf("provision: %v", err)
	}
	app := &config.AppConfig{Env: map[string]string{"DB_URL": config.EnvRef("db", "DATABASE_URL")}}
	if err := BindEnvironment(app, "db", result.Env); err != nil {
		t.Fatal(err)
	}
	if app.Env["MYSQL_PASSWORD"] != config.EnvRef("db", "MYSQL_PASSWORD") || app.Env["DB_URL"] != config.EnvRef("db", "DATABASE_URL") {
		t.Fatal("plugin env did not bind app aliases and canonical keys")
	}
	if err := a.(*Plugin).Deprovision(ctx, instance, true); err != nil {
		t.Fatal(err)
	}
	deletion := <-server.deleted
	if !deletion.KeepData || !proto.Equal(deletion.Instance, instance) {
		t.Fatal("deletion lost identity or retention request")
	}
	closePlugins()
	if _, err := Get(server.typ); err == nil {
		t.Fatal("shutdown left a closed plugin registered")
	}
}

func TestPluginStartupRollsBackAndRejectsBuiltinOverrides(t *testing.T) {
	before := Types()
	server := &testPluginServer{typ: "firstplugin"}
	address := startPlugin(t, server)
	_, err := ConnectPlugins(context.Background(), config.AddonPlugins{
		{Type: "firstplugin", Address: address, Insecure: true},
		{Type: "differenttype", Address: address, Insecure: true},
	})
	if err == nil {
		t.Fatal("accepted mismatched descriptor")
	}
	if !reflect.DeepEqual(Types(), before) {
		t.Fatal("failed startup left partial catalog registration")
	}
	_, err = ConnectPlugins(context.Background(), config.AddonPlugins{{Type: "postgres", Address: address, Insecure: true}})
	if err == nil {
		t.Fatal("plugin replaced a built-in addon")
	}
}

func TestPluginPropagatesRPCFailures(t *testing.T) {
	server := &testPluginServer{typ: "failedplugin", provision: func(context.Context, *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error) {
		return nil, status.Error(codes.Unavailable, "backend offline")
	}}
	address := startPlugin(t, server)
	closePlugins, err := ConnectPlugins(context.Background(), config.AddonPlugins{{Type: server.typ, Address: address, Insecure: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer closePlugins()
	a, _ := Get(server.typ)
	if _, err := a.(*Plugin).Provision(context.Background(), &addonv1.ProvisionRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("lost gRPC failure: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.(*Plugin).Provision(ctx, &addonv1.ProvisionRequest{}); status.Code(err) != codes.Canceled {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestProvisioningRejectsInvalidBindingsAndNetworkDestinations(t *testing.T) {
	for name, mutate := range map[string]func(*addonv1.ProvisionResponse){
		"missing env":       func(p *addonv1.ProvisionResponse) { p.Env = nil },
		"invalid env key":   func(p *addonv1.ProvisionResponse) { p.Env["BAD-KEY"] = "secret" },
		"nul env":           func(p *addonv1.ProvisionResponse) { p.Env["MYSQL_PASSWORD"] = "secret\x00" },
		"missing endpoints": func(p *addonv1.ProvisionResponse) { p.Endpoints = nil },
		"nil endpoint":      func(p *addonv1.ProvisionResponse) { p.Endpoints[0] = nil },
		"URL host":          func(p *addonv1.ProvisionResponse) { p.Endpoints[0].Host = "mysql://secret@db" },
		"bad port":          func(p *addonv1.ProvisionResponse) { p.Endpoints[0].Port = 0 },
		"bad protocol":      func(p *addonv1.ProvisionResponse) { p.Endpoints[0].Protocol = "HTTP" },
		"no namespace":      func(p *addonv1.ProvisionResponse) { p.Endpoints[0].Kubernetes.Namespace = "" },
		"all pods":          func(p *addonv1.ProvisionResponse) { p.Endpoints[0].Kubernetes.PodLabels = nil },
		"two targets":       func(p *addonv1.ProvisionResponse) { p.Endpoints[0].Cidrs = []string{"192.0.2.1/32"} },
		"bad CIDR": func(p *addonv1.ProvisionResponse) {
			p.Endpoints[0].Kubernetes = nil
			p.Endpoints[0].Cidrs = []string{"secret"}
		},
		"all internet": func(p *addonv1.ProvisionResponse) {
			p.Endpoints[0].Kubernetes = nil
			p.Endpoints[0].Cidrs = []string{"0.0.0.0/0"}
		},
		"invalid URL": func(p *addonv1.ProvisionResponse) { p.Urls = []string{"mysql://secret@:3306/app"} },
	} {
		t.Run(name, func(t *testing.T) {
			p := sampleProvisioning()
			mutate(p)
			err := ValidateProvisioning(p)
			if err == nil {
				t.Fatal("invalid plugin output accepted")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("credentials leaked into validation error")
			}
		})
	}
}
