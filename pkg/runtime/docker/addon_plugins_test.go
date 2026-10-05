package docker

import (
	"context"
	"net"
	"net/http"
	"testing"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	addonv1 "github.com/adrianliechti/donkey/pkg/addons/v1"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/docker/docker/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type dockerPluginServer struct {
	addonv1.UnimplementedServiceProviderServer
	deletions chan *addonv1.DeprovisionRequest
}

func (*dockerPluginServer) Offerings(context.Context, *addonv1.OfferingsRequest) (*addonv1.OfferingsResponse, error) {
	return &addonv1.OfferingsResponse{Offerings: []*addonv1.Offering{{
		Id: "docker-testdb", Protocol: "mysql", Title: "Remote DB", Category: "database",
		Parameters:   addons.MustStruct(addons.ObjectSchema(map[string]any{addons.ParamVersion: addons.VersionParameter("1", "tag")})),
		Env:          []*addonv1.EnvVar{{Name: "DATABASE_URL", Secret: true}},
		Capabilities: &addonv1.Capabilities{RetainData: true},
	}}}, nil
}

func (*dockerPluginServer) Provision(r *addonv1.ProvisionRequest, stream addonv1.ServiceProvider_ProvisionServer) error {
	if r.Instance.Kubernetes != nil || r.Instance.Offering != "docker-testdb" || r.Instance.Installation != dockerInstallation {
		return status.Error(codes.InvalidArgument, "wrong identity")
	}
	return stream.Send(&addonv1.ProvisionEvent{Result: &addonv1.ProvisionResponse{
		Env:       map[string]string{"DATABASE_URL": "mysql://donkey:secret@db:3306/app"},
		Endpoints: []*addonv1.Endpoint{{Host: "db", Port: 3306}},
	}})
}

func (p *dockerPluginServer) Deprovision(_ context.Context, r *addonv1.DeprovisionRequest) (*addonv1.DeprovisionResponse, error) {
	p.deletions <- r
	return &addonv1.DeprovisionResponse{}, nil
}

func TestDockerPluginRetainsAddressesAndLifecycleState(t *testing.T) {
	ctx := context.Background()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	remote := &dockerPluginServer{deletions: make(chan *addonv1.DeprovisionRequest, 4)}
	addonv1.RegisterServiceProviderServer(s, remote)
	go s.Serve(listener)
	defer s.Stop()
	closePlugins, err := addons.ConnectPlugins(config.AddonPlugins{{Name: "remote", Address: listener.Addr().String(), Insecure: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer closePlugins()
	cli, err := client.NewClientWithOpts(client.WithHost("http://docker.test"), client.WithVersion("1.48"), client.WithHTTPClient(&http.Client{Transport: dockerRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return dockerJSONResponse(http.StatusNotFound, map[string]string{"message": "not found"}), nil
	})}))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	r := &Runtime{cli: cli, state: state{dir: t.TempDir()}}
	a, err := addons.Get(ctx, "docker-testdb")
	if err != nil {
		t.Fatal(err)
	}
	plugin := a.(*addons.Plugin)
	desc, err := plugin.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	spec := rt.ServiceSpec{Type: "docker-testdb", Name: "db"}
	binding, params, err := r.provisionPlugin(ctx, "team", spec, plugin, desc, nil)
	if err != nil || params["version"] != "1" {
		t.Fatalf("provision: %v %v", params, err)
	}
	got, err := r.ServiceBinding(ctx, "team", sharedOwner, "db")
	if err != nil || got["DATABASE_URL"] != binding["DATABASE_URL"] {
		t.Fatal("Docker rewrote a plugin's remote address into a local container name")
	}
	services, err := r.ListServices(ctx, "team", sharedOwner)
	if err != nil || len(services) != 1 || services[0].Protocol != "mysql" || services[0].Type != "docker-testdb" || services[0].Parameters["version"] != "1" || services[0].State != "" {
		t.Fatalf("lost service metadata: %+v %v", services, err)
	}
	if connection, err := r.ServiceConnection(ctx, "team", "db"); err != nil || connection.Parameters["version"] != "1" {
		t.Fatalf("connection lost parameters: %+v %v", connection, err)
	}
	if host, port, err := r.ServiceEndpoint(ctx, "team", "db"); err != nil || host != "db" || port != 3306 {
		t.Fatal("lost remote endpoint")
	}
	if _, err := r.RemoveService(ctx, "team", "db", true, false); err != nil {
		t.Fatal(err)
	}
	if !(<-remote.deletions).KeepData {
		t.Fatal("lost retention flag")
	}
	stored, err := r.pluginState("team", "db")
	if err != nil || stored == nil || !stored.Stopped {
		t.Fatal("lost retained state")
	}
	if values, err := r.ServiceBinding(ctx, "team", sharedOwner, "db"); err != nil || len(values) != 0 {
		t.Fatal("stopped binding prevents re-provisioning on the next deploy")
	}
	if services, err := r.ListServices(ctx, "team", sharedOwner); err != nil || len(services) != 1 || services[0].State != addons.StateStopped {
		t.Fatalf("stopped instance not listed: %+v %v", services, err)
	}
	if _, _, err := r.provisionPlugin(ctx, "team", spec, plugin, desc, stored); err != nil {
		t.Fatal(err)
	}
	if err := r.deprovisionSpacePlugins(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	if deletion := <-remote.deletions; deletion.KeepData || deletion.Instance.Name != "db" {
		t.Fatal("space deletion lost plugin identity")
	}
	if _, err := r.RemoveService(ctx, "team", "db", false, false); err != nil {
		t.Fatal(err)
	}
	<-remote.deletions
	if state, err := r.pluginState("team", "db"); err != nil || state != nil {
		t.Fatal("full deletion retained provisioning state")
	}
	if _, _, err := r.provisionPlugin(ctx, "team", spec, plugin, desc, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RemoveService(ctx, "team", "db", true, true); err == nil {
		t.Fatal("orphaning with keep-data would retain the lifecycle record")
	}
	if _, err := r.RemoveService(ctx, "team", "db", false, true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-remote.deletions:
		t.Fatal("orphaning deprovisioned the backing service")
	default:
	}
	if state, err := r.pluginState("team", "db"); err != nil || state != nil {
		t.Fatal("orphaned service retained its lifecycle record")
	}
}
