package addons

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type catalogClient struct {
	addonv1.ServiceProviderClient
	answer func(context.Context) (*addonv1.OfferingsResponse, error)
}

func (c catalogClient) Offerings(ctx context.Context, _ *addonv1.OfferingsRequest, _ ...grpc.CallOption) (*addonv1.OfferingsResponse, error) {
	return c.answer(ctx)
}

func TestCatalogRowsDoNotWaitAgainOrChangeDuringRefresh(t *testing.T) {
	p := &Provider{name: "slow", inflight: make(chan struct{})}
	row := p.entries(context.Background(), 0)[0]
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := row.Describe(ctx); err == nil {
		t.Fatal("an unanswered provider appeared available")
	}
	if ctx.Err() != nil {
		t.Fatal("describing a catalog row waited for the provider a second time")
	}
	o := testOffering("firstdb")
	p.offerings = []*addonv1.Offering{o}
	row = p.entries(ctx, 0)[0]
	resolved := p.offering(ctx, "firstdb", 0)
	p.offerings = []*addonv1.Offering{testOffering("seconddb")}
	if desc, err := row.Describe(ctx); err != nil || desc.Id != "firstdb" {
		t.Fatalf("a catalog refresh changed a row already being rendered: %v %v", desc, err)
	}
	if desc, err := resolved.Describe(ctx); err != nil || desc.Id != "firstdb" {
		t.Fatalf("a catalog refresh changed a resolved provisioning contract: %v %v", desc, err)
	}
}

func TestConcurrentCatalogsCannotClaimTheSameOffering(t *testing.T) {
	const count = 4
	arrived, release := make(chan struct{}, count), make(chan struct{})
	var list []*Provider
	for n := range count {
		offerings := []*addonv1.Offering{testOffering("collisiondb")}
		// The shared id is checked before the rest of each catalog. Without
		// atomic validation/publication, several providers can claim it.
		for i := range 50 {
			offerings = append(offerings, testOffering(fmt.Sprintf("provider%d-db%d", n, i)))
		}
		list = append(list, &Provider{name: fmt.Sprintf("provider%d", n), client: catalogClient{answer: func(ctx context.Context) (*addonv1.OfferingsResponse, error) {
			arrived <- struct{}{}
			select {
			case <-release:
				return &addonv1.OfferingsResponse{Offerings: offerings}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}}})
	}
	providersMu.Lock()
	before := providers
	providers = list
	providersMu.Unlock()
	t.Cleanup(func() {
		providersMu.Lock()
		providers = before
		providersMu.Unlock()
	})
	var done []chan struct{}
	for _, p := range list {
		p.refresh(context.Background(), 0)
		p.mu.Lock()
		done = append(done, p.inflight)
		p.mu.Unlock()
	}
	for range count {
		<-arrived
	}
	close(release)
	for _, ch := range done {
		<-ch
	}
	owners := 0
	for _, p := range list {
		if len(p.knownIDs()) != 0 {
			owners++
		}
	}
	if owners != 1 {
		t.Fatalf("%d providers claimed the same offering", owners)
	}
}

func TestExistingInstanceUsesItsRecordedContract(t *testing.T) {
	server := &testProviderServer{offerings: []*addonv1.Offering{testOffering("recordeddb")}}
	connectTestProvider(t, "recorded", server)
	desc := testOffering("recordeddb")
	desc.Parameters = MustStruct(ObjectSchema(map[string]any{ParamVersion: VersionParameter("11.4", "original")}))
	previous := &Provisioning{Type: desc.Id, Provider: "recorded", Offering: desc, Parameters: Parameters{ParamVersion: "11.4"}}
	a, err := Lookup(context.Background(), desc.Id, previous)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := a.Describe(context.Background()); err != nil || !proto.Equal(got, desc) {
		t.Fatalf("instance lost its recorded contract: %v %v", got, err)
	}
	if calls := server.calls.Load(); calls != 0 {
		t.Fatalf("existing instance made %d catalog calls", calls)
	}
}

func TestDeprecatedOfferingsRejectNewInstances(t *testing.T) {
	desc := testOffering("retireddb")
	desc.Deprecated = true
	if _, _, err := PlanProvision(desc, nil, nil); err == nil {
		t.Fatal("a new instance used a deprecated offering")
	}
	if _, _, err := PlanProvision(desc, nil, &Provisioning{Stopped: true}); err != nil {
		t.Fatalf("an existing deprecated instance could not resume: %v", err)
	}
	desc.Parameters, desc.Capabilities = nil, nil
	if _, _, err := PlanProvision(desc, nil, &Provisioning{Info: sampleProvisioning()}); err != nil {
		t.Fatalf("an unchanged instance without parameters needs no update capability: %v", err)
	}
}

func TestProviderValidatesEffectiveParameters(t *testing.T) {
	response := sampleProvisioning()
	response.Parameters = Parameters{ParamStorage: "2Gi"}.Struct()
	server := &testProviderServer{offerings: []*addonv1.Offering{testOffering("effectivedb")}, provision: func(context.Context, *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error) {
		return response, nil
	}}
	connectTestProvider(t, "effective", server)
	p := getPlugin(t, "effectivedb")
	req := &addonv1.ProvisionRequest{Parameters: Parameters{ParamVersion: "11.4", ParamStorage: "1Gi"}.Struct()}
	result, err := p.Provision(context.Background(), req, nil)
	if err != nil || result.GetParameters().AsMap()[ParamVersion] != "11.4" || result.GetParameters().AsMap()[ParamStorage] != "2Gi" {
		t.Fatalf("effective parameters did not overlay the request: %v %v", result, err)
	}
	response.Parameters = Parameters{ParamStorage: "invalid"}.Struct()
	if _, err := p.Provision(context.Background(), req, nil); err == nil {
		t.Fatal("accepted effective parameters rejected by the schema")
	}
}

type testProviderServer struct {
	addonv1.UnimplementedServiceProviderServer
	offerings []*addonv1.Offering
	messages  []string
	provision func(context.Context, *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error)
	deleted   chan *addonv1.DeprovisionRequest
	calls     atomic.Int32

	mu           sync.Mutex
	offeringsErr error
}

func (s *testProviderServer) failOfferings(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offeringsErr = err
}

func (s *testProviderServer) Offerings(context.Context, *addonv1.OfferingsRequest) (*addonv1.OfferingsResponse, error) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.offeringsErr != nil {
		return nil, s.offeringsErr
	}
	return &addonv1.OfferingsResponse{Offerings: s.offerings}, nil
}

func (s *testProviderServer) Provision(req *addonv1.ProvisionRequest, stream addonv1.ServiceProvider_ProvisionServer) error {
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

func (s *testProviderServer) Deprovision(_ context.Context, r *addonv1.DeprovisionRequest) (*addonv1.DeprovisionResponse, error) {
	s.deleted <- r
	return &addonv1.DeprovisionResponse{}, nil
}

// testOffering is a MySQL-speaking database with a version and a disk.
func testOffering(id string) *addonv1.Offering {
	return &addonv1.Offering{
		Id: id, Title: "Test DB", Protocol: "mysql", Category: "database", Tags: []string{"sql"},
		Parameters: MustStruct(versionSchema()),
		Env: []*addonv1.EnvVar{
			{Name: "DATABASE_URL", Secret: true},
			{Name: "MYSQL_PASSWORD", Secret: true},
			{Name: "MYSQL_NOTE", Optional: true},
		},
		Capabilities: &addonv1.Capabilities{RetainData: true, Update: true},
	}
}

func startProvider(t *testing.T, server *testProviderServer) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	addonv1.RegisterServiceProviderServer(grpcServer, server)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)
	return listener.Addr().String()
}

// connectTestProvider registers a provider with no cache TTL and no
// backoff, so every call sees the server's current answer.
func connectTestProvider(t *testing.T, name string, server *testProviderServer) {
	t.Helper()
	ttl, backoff := offeringsTTL, backoffMin
	offeringsTTL, backoffMin = 0, 0
	t.Cleanup(func() { offeringsTTL, backoffMin = ttl, backoff })
	closePlugins, err := ConnectPlugins(config.AddonPlugins{{Name: name, Address: startProvider(t, server), Insecure: true}})
	if err != nil {
		t.Fatal(err)
	}
	p := providerNamed(name)
	t.Cleanup(func() {
		closePlugins()
		// A stale-cache refresh may still be recording its failure after
		// Close. Join it before restoring the test's TTL/backoff settings.
		p.mu.Lock()
		done := p.inflight
		p.mu.Unlock()
		if done != nil {
			<-done
		}
	})
	if server.calls.Load() != 0 {
		t.Fatal("registering a provider must not call it")
	}
}

func getPlugin(t *testing.T, id string) *Plugin {
	t.Helper()
	a, err := Get(context.Background(), id)
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
		Env: map[string]string{"DATABASE_URL": "mysql://donkey:secret@db.addons.svc:3306/app", "MYSQL_PASSWORD": "secret"},
		Endpoints: []*addonv1.Endpoint{{Host: "db.addons.svc", Port: 3306, Target: &addonv1.Endpoint_Kubernetes{
			Kubernetes: &addonv1.KubernetesTarget{Namespace: "addons", PodLabels: map[string]string{"instance": "team-db"}},
		}}},
	}
}

func TestProviderCatalogLifecycleAndEnvironment(t *testing.T) {
	ctx := context.Background()
	instance := &addonv1.Instance{Installation: "main", Space: "team", Name: "db", Offering: "testdb",
		Kubernetes: &addonv1.KubernetesContext{Namespace: "workloads", ConsumerLabels: map[string]string{"space": "team"}}}
	want := sampleProvisioning()
	api := &addonv1.Offering{Id: "testapi", Title: "Test API", Protocol: "openai", Category: "api", Shared: true,
		Env: []*addonv1.EnvVar{{Name: "OPENAI_BASE_URL"}, {Name: "OPENAI_API_KEY", Secret: true}}}
	server := &testProviderServer{offerings: []*addonv1.Offering{testOffering("testdb"), api}, messages: []string{"creating", "waiting"}, deleted: make(chan *addonv1.DeprovisionRequest, 1),
		provision: func(ctx context.Context, req *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error) {
			if _, ok := ctx.Deadline(); !ok {
				return nil, status.Error(codes.Internal, "missing deadline")
			}
			if !proto.Equal(instance, req.Instance) || req.Parameters.AsMap()[ParamVersion] != "11.8" || req.Reason != addonv1.ProvisionReason_PROVISION_REASON_CREATE {
				return nil, status.Error(codes.InvalidArgument, "identity, parameters or reason lost")
			}
			return want, nil
		}}
	connectTestProvider(t, "acme", server)
	// One provider, two catalog entries; the built-ins stay in front.
	var ids []string
	for _, a := range Types(ctx) {
		ids = append(ids, a.Type())
	}
	if !reflect.DeepEqual(ids, []string{"openai", "postgres", "redis", "rustfs", "testdb", "testapi"}) {
		t.Fatalf("catalog: %v", ids)
	}
	p := getPlugin(t, "testdb")
	desc, err := p.Describe(ctx)
	if err != nil || desc.Protocol != "mysql" || !desc.Capabilities.RetainData || p.Provider() != "acme" {
		t.Fatalf("descriptor: %+v %v", desc, err)
	}
	if shared, err := getPlugin(t, "testapi").Describe(ctx); err != nil || !shared.Shared {
		t.Fatalf("second offering lost: %v", err)
	}
	if template := EnvTemplate(desc); len(template) != 3 || template["DATABASE_URL"] != "" {
		t.Fatal("catalog must return keys without credentials")
	}
	params, reason, err := PlanProvision(desc, map[string]any{ParamVersion: "11.8"}, nil)
	if err != nil || params.String(ParamStorage) != "1Gi" || reason != addonv1.ProvisionReason_PROVISION_REASON_CREATE {
		t.Fatalf("defaults not applied: %v %v", params, err)
	}
	var progress []string
	result, err := p.Provision(ctx, &addonv1.ProvisionRequest{Instance: instance, Parameters: params.Struct(), Reason: reason}, func(m string) { progress = append(progress, m) })
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
	// Removal goes through the record's pinned offering: the provider's
	// catalog is not consulted, even when it is gone.
	record := &Provisioning{Type: "testdb", Provider: "acme", Instance: instance, Offering: desc, Info: result, Parameters: params}
	server.failOfferings(status.Error(codes.Unavailable, "restarting"))
	pinned, err := record.Plugin()
	if err != nil {
		t.Fatal(err)
	}
	if err := pinned.Deprovision(ctx, instance, true); err != nil {
		t.Fatal(err)
	}
	deletion := <-server.deleted
	if !deletion.KeepData || !proto.Equal(deletion.Instance, instance) {
		t.Fatal("deletion lost identity or retention request")
	}
	if params, reason, err := PlanProvision(desc, nil, &Provisioning{Stopped: true, Parameters: params}); err != nil || reason != addonv1.ProvisionReason_PROVISION_REASON_RESUME || params.String(ParamVersion) != "11.8" {
		t.Fatalf("resume: %v %v %v", params, reason, err)
	}
}

func TestProviderJoinsCatalogWhenItStartsAnswering(t *testing.T) {
	server := &testProviderServer{offerings: []*addonv1.Offering{testOffering("latedb")}, deleted: make(chan *addonv1.DeprovisionRequest, 1)}
	server.failOfferings(status.Error(codes.Unavailable, "still starting"))
	connectTestProvider(t, "late", server)
	ctx := context.Background()
	entries := Types(ctx)
	placeholder := entries[len(entries)-1]
	if placeholder.Type() != "late" {
		t.Fatalf("an unanswered provider must be listed under its name: %v", placeholder.Type())
	}
	if _, err := placeholder.Describe(ctx); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("unanswered provider reported as available: %v", err)
	}
	if _, err := Get(ctx, "latedb"); err == nil {
		t.Fatal("resolved an offering nobody has described")
	}
	server.failOfferings(nil)
	p := getPlugin(t, "latedb")
	if desc, err := p.Describe(ctx); err != nil || desc.Id != "latedb" {
		t.Fatalf("provider did not join once answering: %v", err)
	}
	// A later outage keeps the last good catalog.
	server.failOfferings(status.Error(codes.Unavailable, "restarting"))
	if desc, err := p.Describe(ctx); err != nil || desc.Id != "latedb" {
		t.Fatalf("transient outage dropped the catalog entry: %v", err)
	}
	if _, err := Get(ctx, "latedb"); err != nil {
		t.Fatalf("transient outage broke resolution: %v", err)
	}
}

func TestProviderBackoffAndListingsDoNotBlockOnADeadProvider(t *testing.T) {
	server := &testProviderServer{offerings: []*addonv1.Offering{testOffering("slowdb")}}
	server.failOfferings(status.Error(codes.Unavailable, "down"))
	connectTestProvider(t, "slow", server)
	backoffMin = time.Hour
	ctx := context.Background()
	Types(ctx)
	Types(ctx)
	if _, err := Get(ctx, "slowdb"); err == nil {
		t.Fatal("resolved through a dead provider")
	}
	if calls := server.calls.Load(); calls != 1 {
		t.Fatalf("a failing provider was probed %d times within its backoff", calls)
	}
	// With a cached catalog, a listing never waits: the refresh runs behind it.
	backoffMin = 0
	provider := providerNamed("slow")
	provider.mu.Lock()
	provider.nextTry = time.Time{}
	provider.mu.Unlock()
	server.failOfferings(nil)
	getPlugin(t, "slowdb")
	server.failOfferings(status.Error(codes.Unavailable, "down again"))
	started := time.Now()
	for range 5 {
		Types(ctx)
	}
	if time.Since(started) > catalogWait {
		t.Fatal("listing waited on a provider with a cached catalog")
	}
}

func TestProviderRegistrationRejectsReservedAndDuplicateNames(t *testing.T) {
	before := Types(context.Background())
	server := &testProviderServer{offerings: []*addonv1.Offering{testOffering("firstdb")}}
	address := startProvider(t, server)
	if _, err := ConnectPlugins(config.AddonPlugins{{Name: "postgres", Address: address, Insecure: true}}); err == nil {
		t.Fatal("a provider took a built-in's name")
	}
	if _, err := ConnectPlugins(config.AddonPlugins{
		{Name: "first", Address: address, Insecure: true},
		{Name: "first", Address: address, Insecure: true},
	}); err == nil {
		t.Fatal("accepted a duplicate name")
	}
	if !reflect.DeepEqual(Types(context.Background()), before) {
		t.Fatal("failed startup left partial catalog registration")
	}
}

func TestProviderCatalogValidation(t *testing.T) {
	ctx := context.Background()
	for name, offerings := range map[string][]*addonv1.Offering{
		"empty":        {},
		"builtin id":   {testOffering("postgres")},
		"duplicate id": {testOffering("dupdb"), testOffering("dupdb")},
		"bad id":       {testOffering("Bad DB")},
		"no env":       {{Id: "noenv", Title: "x", Protocol: "custom"}},
		"missing url":  {{Id: "nourl", Title: "x", Protocol: "mysql", Env: []*addonv1.EnvVar{{Name: "MYSQL_PASSWORD"}}}},
		"bad category": {func() *addonv1.Offering { o := testOffering("catdb"); o.Category = "Data Base"; return o }()},
		"bad docs url": {func() *addonv1.Offering { o := testOffering("docdb"); o.DocumentationUrl = "ftp://x"; return o }()},
		"bad schema": {func() *addonv1.Offering {
			o := testOffering("schemadb")
			o.Parameters = MustStruct(map[string]any{"type": "array"})
			return o
		}()},
		"bad env key": {func() *addonv1.Offering { o := testOffering("envdb"); o.Env[0].Name = "DATABASE-URL"; return o }()},
	} {
		t.Run(name, func(t *testing.T) {
			server := &testProviderServer{offerings: offerings}
			connectTestProvider(t, "invalid", server)
			entries := Types(ctx)
			if _, err := entries[len(entries)-1].Describe(ctx); err == nil {
				t.Fatal("accepted an invalid catalog")
			}
		})
	}
	// Two providers must not offer the same id: the second answer is refused.
	connectTestProvider(t, "one", &testProviderServer{offerings: []*addonv1.Offering{testOffering("shareddb")}})
	getPlugin(t, "shareddb")
	connectTestProvider(t, "two", &testProviderServer{offerings: []*addonv1.Offering{testOffering("shareddb")}})
	if p := getPlugin(t, "shareddb"); p.Provider() != "one" {
		t.Fatalf("offering resolved to the colliding provider %s", p.Provider())
	}
}

func TestProviderPropagatesRPCFailures(t *testing.T) {
	server := &testProviderServer{offerings: []*addonv1.Offering{testOffering("faileddb")}, provision: func(context.Context, *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error) {
		return nil, status.Error(codes.Unavailable, "backend offline")
	}}
	connectTestProvider(t, "failing", server)
	p := getPlugin(t, "faileddb")
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

func TestProviderEnforcesEnvContractAndCapabilities(t *testing.T) {
	response := sampleProvisioning()
	offering := testOffering("contractdb")
	offering.Capabilities = nil
	server := &testProviderServer{offerings: []*addonv1.Offering{offering}, provision: func(context.Context, *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error) {
		return response, nil
	}}
	connectTestProvider(t, "contract", server)
	p := getPlugin(t, "contractdb")
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
		t.Fatalf("keep_data sent to an offering without the capability: %v", err)
	}
	desc, _ := p.Describe(ctx)
	ready := &Provisioning{Parameters: Parameters{ParamVersion: "11.8", ParamStorage: "1Gi"}, Info: response}
	if _, reason, err := PlanProvision(desc, map[string]any{ParamVersion: "11.4"}, ready); err == nil || reason != addonv1.ProvisionReason_PROVISION_REASON_UPDATE {
		t.Fatalf("changed parameters accepted without the update capability: %v", err)
	}
	if params, _, err := PlanProvision(desc, nil, ready); err != nil || params.String(ParamVersion) != "11.8" {
		t.Fatalf("a repeat with unchanged parameters must pass: %v", err)
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
		Type: "mariadb", Provider: "acme", Protocol: "mysql",
		Instance:   &addonv1.Instance{Installation: "main", Space: "team", Name: "db", Offering: "mariadb", Kubernetes: &addonv1.KubernetesContext{Namespace: "ns"}, Labels: map[string]string{"owner": "x"}},
		Offering:   testOffering("mariadb"),
		Parameters: Parameters{ParamVersion: "11.8", "replicas": float64(2)},
		Info:       sampleProvisioning(),
		Pending:    true,
		Error:      "last failure",
	}
	record.Info.Parameters = Parameters{ParamStorage: "2Gi"}.Struct()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sizeCache") || !strings.Contains(string(raw), `"podLabels"`) || !strings.Contains(string(raw), `"retainData"`) {
		t.Fatalf("record is not proto JSON: %s", raw)
	}
	var back Provisioning
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(back.Instance, record.Instance) || !proto.Equal(back.Info, record.Info) || !proto.Equal(back.Offering, record.Offering) || !reflect.DeepEqual(back.Parameters, record.Parameters) || back.Error != record.Error || back.Protocol != "mysql" || back.Provider != "acme" || !back.Pending {
		t.Fatalf("round trip lost data: %+v", back)
	}
	if back.State() != StateFailed || (&Provisioning{Stopped: true}).State() != StateStopped || (&Provisioning{}).State() != StatePending || (&Provisioning{Info: sampleProvisioning()}).State() != StateReady {
		t.Fatal("states misclassified")
	}
	if back.Reason() != addonv1.ProvisionReason_PROVISION_REASON_RETRY || (&Provisioning{Stopped: true}).Reason() != addonv1.ProvisionReason_PROVISION_REASON_RESUME || (&Provisioning{Info: sampleProvisioning()}).Reason() != addonv1.ProvisionReason_PROVISION_REASON_UPDATE || (*Provisioning)(nil).Reason() != addonv1.ProvisionReason_PROVISION_REASON_CREATE {
		t.Fatal("reasons misclassified")
	}
	if host, port := back.Endpoint(); host != "db.addons.svc" || port != 3306 {
		t.Fatal("primary endpoint lost")
	}
	if _, err := back.Plugin(); err == nil || !strings.Contains(err.Error(), "no longer has its provider") {
		t.Fatalf("record resolved without its provider: %v", err)
	}
}

func TestInterruptedUpdateRetriesWithItsPreviousResult(t *testing.T) {
	record := &Provisioning{
		Instance:   &addonv1.Instance{Installation: "main", Space: "team", Name: "db", Offering: "db"},
		Parameters: Parameters{ParamVersion: "11.4", ParamStorage: "1Gi"}, Info: sampleProvisioning(), Pending: true,
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var restored Provisioning
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.State() != StatePending || restored.Reason() != addonv1.ProvisionReason_PROVISION_REASON_RETRY || !proto.Equal(restored.Info, record.Info) {
		t.Fatal("an interrupted update appeared ready or lost its previous response")
	}
	if _, reason, err := PlanProvision(testOffering("db"), nil, &restored); err != nil || reason != addonv1.ProvisionReason_PROVISION_REASON_RETRY {
		t.Fatalf("interrupted update did not plan a retry: %v %v", reason, err)
	}
}
