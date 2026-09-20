package addons

import (
	"context"
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"sync"
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
	typ          string
	protocol     string
	schema       map[string]any
	capabilities []string
	messages     []string
	provision    func(context.Context, *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error)
	deleted      chan *addonv1.DeprovisionRequest

	mu          sync.Mutex
	describeErr error
}

func (s *testPluginServer) failDescribe(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.describeErr = err
}

func (s *testPluginServer) Describe(context.Context, *addonv1.DescribeRequest) (*addonv1.DescribeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.describeErr != nil {
		return nil, s.describeErr
	}
	protocol := s.protocol
	if protocol == "" {
		protocol = "mysql"
	}
	capabilities := s.capabilities
	if capabilities == nil {
		capabilities = []string{CapabilityKeepData}
	}
	desc := &addonv1.DescribeResponse{
		Type: s.typ, Protocol: protocol, Title: "Test DB",
		Env: []*addonv1.EnvVar{
			{Name: "DATABASE_URL", Secret: true},
			{Name: "MYSQL_PASSWORD", Secret: true},
			{Name: "MYSQL_NOTE", Optional: true},
		},
		Capabilities: capabilities,
	}
	if s.schema != nil {
		desc.Parameters = MustStruct(s.schema)
	}
	return desc, nil
}

func (s *testPluginServer) Provision(req *addonv1.ProvisionRequest, stream addonv1.AddonPlugin_ProvisionServer) error {
	for _, message := range s.messages {
		if err := stream.Send(&addonv1.ProvisionEvent{Message: message}); err != nil {
			return err
		}
	}
	result, err := s.provision(stream.Context(), req)
	if err != nil {
		return err
	}
	if result == nil {
		return nil // ends without a result
	}
	return stream.Send(&addonv1.ProvisionEvent{Result: result})
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

func connectTestPlugin(t *testing.T, server *testPluginServer) *Plugin {
	t.Helper()
	closePlugins, err := ConnectPlugins(context.Background(), config.AddonPlugins{{Type: server.typ, Address: startPlugin(t, server), Insecure: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closePlugins)
	a, err := Get(server.typ)
	if err != nil {
		t.Fatal(err)
	}
	return a.(*Plugin)
}

func versionSchema() map[string]any {
	return ObjectSchema(map[string]any{
		ParamVersion: VersionParameter("11.8", "MariaDB release"),
		ParamStorage: StorageParameter("1Gi"),
	})
}

func sampleProvisioning() *addonv1.ProvisionResponse {
	return &addonv1.ProvisionResponse{
		Env:  map[string]string{"DATABASE_URL": "mysql://donkey:secret@db.addons.svc:3306/app", "MYSQL_PASSWORD": "secret"},
		Urls: []string{"mysql://db.addons.svc:3306/app"},
		Endpoints: []*addonv1.Endpoint{{Host: "db.addons.svc", Port: 3306, Target: &addonv1.Endpoint_Kubernetes{
			Kubernetes: &addonv1.KubernetesTarget{Namespace: "addons", PodLabels: map[string]string{"instance": "team-db"}},
		}}},
	}
}

func TestPluginGRPCLifecycleAndEnvironment(t *testing.T) {
	ctx := context.Background()
	instance := &addonv1.Instance{Installation: "main", Space: "team", Name: "db", Runtime: "kubernetes", Namespace: "workloads"}
	want := sampleProvisioning()
	server := &testPluginServer{typ: "testdb", schema: versionSchema(), messages: []string{"creating", "waiting"}, deleted: make(chan *addonv1.DeprovisionRequest, 1),
		provision: func(ctx context.Context, req *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error) {
			if _, ok := ctx.Deadline(); !ok {
				return nil, status.Error(codes.Internal, "missing deadline")
			}
			if !proto.Equal(instance, req.Instance) || req.Parameters.AsMap()[ParamVersion] != "11.8" {
				return nil, status.Error(codes.InvalidArgument, "identity or parameters lost")
			}
			return want, nil
		}}
	p := connectTestPlugin(t, server)
	desc, err := p.Describe(ctx)
	if err != nil || desc.Protocol != "mysql" || !HasCapability(desc, CapabilityKeepData) || !SupportsRuntime(desc, "docker") {
		t.Fatalf("descriptor: %+v %v", desc, err)
	}
	if binding := p.Binding(Input{}); len(binding) != 3 || binding["DATABASE_URL"] != "" {
		t.Fatal("catalog must return keys without credentials")
	}
	params, err := ResolveParameters(desc.Parameters, map[string]any{ParamVersion: "11.8"})
	if err != nil || params.String(ParamStorage) != "1Gi" {
		t.Fatalf("defaults not applied: %v %v", params, err)
	}
	var progress []string
	result, err := p.Provision(ctx, &addonv1.ProvisionRequest{Instance: instance, Parameters: params.Struct()}, func(m string) { progress = append(progress, m) })
	if err != nil || !proto.Equal(result, want) {
		t.Fatalf("provision: %v", err)
	}
	if !reflect.DeepEqual(progress, []string{"creating", "waiting"}) {
		t.Fatalf("progress lost: %v", progress)
	}
	app := &config.AppConfig{Env: map[string]string{"DB_URL": config.EnvRef("db", "DATABASE_URL")}}
	if err := BindEnvironment(app, "db", result.Env); err != nil {
		t.Fatal(err)
	}
	if app.Env["MYSQL_PASSWORD"] != config.EnvRef("db", "MYSQL_PASSWORD") || app.Env["DB_URL"] != config.EnvRef("db", "DATABASE_URL") {
		t.Fatal("plugin env did not bind app aliases and canonical keys")
	}
	if err := p.Deprovision(ctx, instance, true); err != nil {
		t.Fatal(err)
	}
	deletion := <-server.deleted
	if !deletion.KeepData || !proto.Equal(deletion.Instance, instance) {
		t.Fatal("deletion lost identity or retention request")
	}
}

func TestPluginJoinsCatalogWhenItStartsAnswering(t *testing.T) {
	server := &testPluginServer{typ: "lateplugin", deleted: make(chan *addonv1.DeprovisionRequest, 1)}
	server.failDescribe(status.Error(codes.Unavailable, "still starting"))
	p := connectTestPlugin(t, server)
	ctx := context.Background()
	if _, err := p.Describe(ctx); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("unanswered plugin reported as available: %v", err)
	}
	if len(p.Binding(Input{})) != 0 {
		t.Fatal("keys invented for an unknown plugin")
	}
	if _, err := p.Provision(ctx, &addonv1.ProvisionRequest{}, nil); err == nil {
		t.Fatal("provisioned through an unavailable plugin")
	}
	server.failDescribe(nil)
	old := describeTTL
	describeTTL = 0
	defer func() { describeTTL = old }()
	if desc, err := p.Describe(ctx); err != nil || desc.Type != "lateplugin" {
		t.Fatalf("plugin did not join once answering: %v", err)
	}
	// A later outage keeps the last good description in the catalog.
	server.failDescribe(status.Error(codes.Unavailable, "restarting"))
	if desc, err := p.Describe(ctx); err != nil || desc.Type != "lateplugin" {
		t.Fatalf("transient outage dropped the catalog entry: %v", err)
	}
}

func TestPluginStartupRejectsBuiltinOverridesAndDuplicates(t *testing.T) {
	before := Types()
	server := &testPluginServer{typ: "firstplugin"}
	address := startPlugin(t, server)
	if _, err := ConnectPlugins(context.Background(), config.AddonPlugins{{Type: "postgres", Address: address, Insecure: true}}); err == nil {
		t.Fatal("plugin replaced a built-in addon")
	}
	if _, err := ConnectPlugins(context.Background(), config.AddonPlugins{
		{Type: "firstplugin", Address: address, Insecure: true},
		{Type: "firstplugin", Address: address, Insecure: true},
	}); err == nil {
		t.Fatal("accepted a duplicate type")
	}
	if !reflect.DeepEqual(Types(), before) {
		t.Fatal("failed startup left partial catalog registration")
	}
	// A plugin answering with another type is registered under the
	// configured name but never becomes available.
	closePlugins, err := ConnectPlugins(context.Background(), config.AddonPlugins{{Type: "differenttype", Address: address, Insecure: true}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := Get("differenttype")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Describe(context.Background()); err == nil {
		t.Fatal("accepted a descriptor for another type")
	}
	closePlugins()
	if _, err := Get("differenttype"); err == nil {
		t.Fatal("shutdown left a closed plugin registered")
	}
}

func TestPluginPropagatesRPCFailures(t *testing.T) {
	server := &testPluginServer{typ: "failedplugin", provision: func(context.Context, *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error) {
		return nil, status.Error(codes.Unavailable, "backend offline")
	}}
	p := connectTestPlugin(t, server)
	if _, err := p.Provision(context.Background(), &addonv1.ProvisionRequest{}, nil); status.Code(err) != codes.Unavailable {
		t.Fatalf("lost gRPC failure: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Provision(ctx, &addonv1.ProvisionRequest{}, nil); status.Code(err) != codes.Canceled {
		t.Fatalf("lost cancellation: %v", err)
	}
	server.provision = func(context.Context, *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error) { return nil, nil }
	if _, err := p.Provision(context.Background(), &addonv1.ProvisionRequest{}, nil); err == nil || !strings.Contains(err.Error(), "without a result") {
		t.Fatalf("accepted a stream without a result: %v", err)
	}
}

func TestPluginEnforcesEnvContractAndCapabilities(t *testing.T) {
	response := sampleProvisioning()
	server := &testPluginServer{typ: "contractplugin", capabilities: []string{}, provision: func(context.Context, *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error) {
		return response, nil
	}}
	p := connectTestPlugin(t, server)
	ctx := context.Background()
	if _, err := p.Provision(ctx, &addonv1.ProvisionRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	response.Env["MYSQL_NOTE"] = "optional keys may appear"
	if _, err := p.Provision(ctx, &addonv1.ProvisionRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	response.Env["UNDECLARED"] = "x"
	if _, err := p.Provision(ctx, &addonv1.ProvisionRequest{}, nil); err == nil {
		t.Fatal("accepted an undeclared environment key")
	}
	delete(response.Env, "UNDECLARED")
	delete(response.Env, "MYSQL_PASSWORD")
	if _, err := p.Provision(ctx, &addonv1.ProvisionRequest{}, nil); err == nil {
		t.Fatal("accepted a missing required key")
	}
	if err := p.Deprovision(ctx, &addonv1.Instance{}, true); err == nil || !strings.Contains(err.Error(), "does not retain data") {
		t.Fatalf("keep_data sent to a plugin without the capability: %v", err)
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
		"no namespace":      func(p *addonv1.ProvisionResponse) { p.Endpoints[0].GetKubernetes().Namespace = "" },
		"all pods":          func(p *addonv1.ProvisionResponse) { p.Endpoints[0].GetKubernetes().PodLabels = nil },
		"empty cidrs": func(p *addonv1.ProvisionResponse) {
			p.Endpoints[0].Target = &addonv1.Endpoint_Cidrs{Cidrs: &addonv1.CIDRTarget{}}
		},
		"bad CIDR": func(p *addonv1.ProvisionResponse) {
			p.Endpoints[0].Target = &addonv1.Endpoint_Cidrs{Cidrs: &addonv1.CIDRTarget{Ranges: []string{"secret"}}}
		},
		"all internet": func(p *addonv1.ProvisionResponse) {
			p.Endpoints[0].Target = &addonv1.Endpoint_Cidrs{Cidrs: &addonv1.CIDRTarget{Ranges: []string{"0.0.0.0/0"}}}
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
	valid := sampleProvisioning()
	valid.Endpoints[0].Target = &addonv1.Endpoint_Cidrs{Cidrs: &addonv1.CIDRTarget{Ranges: []string{"192.0.2.0/24", "2001:db8::/32"}}}
	if err := ValidateProvisioning(valid); err != nil {
		t.Fatal(err)
	}
}

func TestProvisioningRecordRoundTripsThroughProtojson(t *testing.T) {
	record := Provisioning{
		Type: "mariadb", Protocol: "mysql",
		Instance:   &addonv1.Instance{Installation: "main", Space: "team", Name: "db", Runtime: "kubernetes", Namespace: "ns", Labels: map[string]string{"owner": "x"}},
		Parameters: Parameters{ParamVersion: "11.8", "replicas": float64(2)},
		Info:       sampleProvisioning(),
		Error:      "last failure",
	}
	record.Info.Parameters = Parameters{ParamStorage: "2Gi"}.Struct()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sizeCache") || !strings.Contains(string(raw), `"podLabels"`) {
		t.Fatalf("record is not proto JSON: %s", raw)
	}
	var back Provisioning
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(back.Instance, record.Instance) || !proto.Equal(back.Info, record.Info) || !reflect.DeepEqual(back.Parameters, record.Parameters) || back.Error != record.Error || back.Protocol != "mysql" {
		t.Fatalf("round trip lost data: %+v", back)
	}
	if back.State() != StateFailed || (&Provisioning{Stopped: true}).State() != StateStopped || (&Provisioning{}).State() != StatePending || (&Provisioning{Info: sampleProvisioning()}).State() != StateReady {
		t.Fatal("states misclassified")
	}
	if host, port := back.Endpoint(); host != "db.addons.svc" || port != 3306 {
		t.Fatal("primary endpoint lost")
	}
}
