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
	addonv1.UnimplementedAddonPluginServer
	deletions chan *addonv1.DeprovisionRequest
}

func (*dockerPluginServer) Describe(context.Context, *addonv1.DescribeRequest) (*addonv1.DescribeResponse, error) {
	return &addonv1.DescribeResponse{Type: "docker-testdb", Protocol: "mysql", Title: "Remote DB", DefaultVersion: "1", EnvKeys: []string{"DATABASE_URL"}}, nil
}
func (*dockerPluginServer) Provision(_ context.Context, r *addonv1.ProvisionRequest) (*addonv1.ProvisionResponse, error) {
	if r.Instance.Runtime != "docker" || r.Instance.Namespace != netName("team") {
		return nil, status.Error(codes.InvalidArgument, "wrong runtime or network")
	}
	return &addonv1.ProvisionResponse{Env: map[string]string{"DATABASE_URL": "mysql://donkey:secret@db:3306/app"}, Urls: []string{"mysql://db:3306/app"}, Endpoints: []*addonv1.Endpoint{{Host: "db", Port: 3306}}}, nil
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
	addonv1.RegisterAddonPluginServer(s, remote)
	go s.Serve(listener)
	defer s.Stop()
	closePlugins, err := addons.ConnectPlugins(ctx, config.AddonPlugins{{Type: "docker-testdb", Address: listener.Addr().String(), Insecure: true}})
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
	a, _ := addons.Get("docker-testdb")
	spec := rt.ServiceSpec{Type: "docker-testdb", Name: "db"}
	binding, version, _, err := r.provisionPlugin(ctx, "team", spec, a.(*addons.Plugin), nil)
	if err != nil || version != "1" {
		t.Fatalf("provision: %v", err)
	}
	got, err := r.ServiceBinding(ctx, "team", sharedOwner, "db")
	if err != nil || got["DATABASE_URL"] != binding["DATABASE_URL"] {
		t.Fatal("Docker rewrote a plugin's remote address into a local container name")
	}
	services, err := r.ListServices(ctx, "team", sharedOwner)
	if err != nil || len(services) != 1 || services[0].Protocol != "mysql" || services[0].Type != "docker-testdb" {
		t.Fatalf("lost service metadata: %+v %v", services, err)
	}
	if host, port, err := r.ServiceEndpoint(ctx, "team", "db"); err != nil || host != "db" || port != 3306 {
		t.Fatal("lost remote endpoint")
	}
	if _, err := r.RemoveService(ctx, "team", "db", true); err != nil {
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
	if _, _, _, err := r.provisionPlugin(ctx, "team", spec, a.(*addons.Plugin), stored); err != nil {
		t.Fatal(err)
	}
	if err := r.deprovisionSpacePlugins(ctx, "team"); err != nil {
		t.Fatal(err)
	}
	if deletion := <-remote.deletions; deletion.KeepData || deletion.Instance.Name != "db" {
		t.Fatal("space deletion lost plugin identity")
	}
	if _, err := r.RemoveService(ctx, "team", "db", false); err != nil {
		t.Fatal(err)
	}
	<-remote.deletions
	if state, err := r.pluginState("team", "db"); err != nil || state != nil {
		t.Fatal("full deletion retained provisioning state")
	}
}
