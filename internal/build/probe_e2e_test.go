//go:build e2e

package build

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/platforms"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	gateway "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/util/progress/progressui"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/tonistiigi/fsutil"
)

// Startup verification against a real BuildKit, in the exact shape a deploy
// uses: the dockerfile frontend through the gateway, its result turned back
// into a state, the probe run on top of that.
//
// This is not a unit test's job. The two bugs this path actually had — the
// busybox binary living at /bin inside the mount, and the stock busybox being
// dynamically linked so it cannot exec from a mount in an image with a
// different libc — are both invisible until something runs.
//
//	PROBE_ADDR=tcp://127.0.0.1:1234 go test -tags e2e -run Probe ./internal/build
func TestProbeOnGatewayResult(t *testing.T) {
	addr := os.Getenv("PROBE_ADDR")
	if addr == "" {
		t.Skip("PROBE_ADDR not set")
	}
	p := platforms.MustParse("linux/arm64")
	if v := os.Getenv("PROBE_PLATFORM"); v != "" {
		p = platforms.MustParse(v)
	}
	// alpine (musl) on purpose: a glibc busybox cannot exec here, which is
	// the failure mode this test exists to keep out.
	for name, tc := range map[string]struct {
		cmd     string
		spec    VerifySpec
		wantErr bool
	}{
		"an app that listens passes": {
			cmd: `CMD ["node", "-e", "require('http').createServer((_,r)=>r.end('ok')).listen(8080)"]`,
		},
		"an app that exits fails": {
			cmd: `CMD ["node", "-e", "process.exit(3)"]`, wantErr: true,
		},
		"a live process without a listener fails": {
			cmd: `CMD ["node", "-e", "setInterval(()=>{},1000)"]`, wantErr: true,
		},
		"the wrong port fails": {
			cmd: `CMD ["node", "-e", "require('http').createServer((_,r)=>r.end('ok')).listen(9090)"]`, wantErr: true,
		},
		"IPv4 loopback fails": {
			cmd: `CMD ["node", "-e", "require('http').createServer((_,r)=>r.end('ok')).listen(8080,'127.0.0.1')"]`, wantErr: true,
		},
		"IPv6 loopback fails": {
			cmd: `CMD ["node", "-e", "require('http').createServer((_,r)=>r.end('ok')).listen(8080,'::1')"]`, wantErr: true,
		},
		"an unhealthy HTTP listener fails": {
			cmd:  `CMD ["node", "-e", "require('http').createServer((_,r)=>{r.statusCode=503;r.end('unavailable')}).listen(8080)"]`,
			spec: VerifySpec{HealthcheckPath: "/healthz"}, wantErr: true,
		},
		"runtime command environment and health host are honored": {
			cmd: `CMD ["node", "-e", "process.exit(3)"]`,
			spec: VerifySpec{
				Command: []string{"node", "-e", `require('http').createServer((q,r)=>{r.statusCode=q.url==='/healthz'&&q.headers.host==='app.example.test'&&process.env.MODE==='ready'?200:503;r.end('ok')}).listen(Number(process.env.PORT),'0.0.0.0')`},
				Env:     map[string]string{"MODE": "ready", "PORT": "9090"}, Host: "app.example.test", HealthcheckPath: "/healthz",
			},
		},
		"runtime storage masks image files": {
			cmd:  "RUN echo image-only > /app/required.txt\n" + `CMD ["node", "-e", "require('fs').readFileSync('/app/required.txt');require('http').createServer((_,r)=>r.end('ok')).listen(8080)"]`,
			spec: VerifySpec{StoragePaths: []string{"/app"}}, wantErr: true,
		},
		"nonroot app can write disposable storage": {
			cmd:  "USER node\n" + `CMD ["node", "-e", "require('fs').writeFileSync('/data/note','ok');require('http').createServer((_,r)=>r.end('ok')).listen(8080)"]`,
			spec: VerifySpec{StoragePaths: []string{"/data"}},
		},
	} {
		t.Run(name, func(t *testing.T) { runProbe(t, addr, p, tc.cmd, tc.spec, tc.wantErr) })
	}
}

func runProbe(t *testing.T, addr string, p ocispecs.Platform, cmd string, spec VerifySpec, wantErr bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"),
		[]byte("FROM node:22-alpine\nWORKDIR /app\n"+cmd+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, err := fsutil.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}

	c, err := bkclient.New(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ch := make(chan *bkclient.SolveStatus)
	go func() {
		d, _ := progressui.NewDisplay(os.Stderr, progressui.PlainMode)
		_, _ = d.UpdateFrom(context.Background(), ch)
	}()

	opt := bkclient.SolveOpt{LocalMounts: map[string]fsutil.FS{"context": fs, "dockerfile": fs}}
	verified := false
	_, err = c.Build(ctx, opt, "probe", func(ctx context.Context, gw gateway.Client) (*gateway.Result, error) {
		res, err := gw.Solve(ctx, gateway.SolveRequest{
			Frontend:    "dockerfile.v0",
			FrontendOpt: map[string]string{"filename": "Dockerfile", "platform": platforms.Format(p)},
		})
		if err != nil {
			return nil, err
		}
		ref, err := res.SingleRef()
		if err != nil {
			return nil, err
		}
		cfg, err := imageConfig(res.Metadata[exptypes.ExporterImageConfigKey])
		if err != nil {
			return nil, err
		}
		app, err := ref.ToState()
		if err != nil {
			return nil, err
		}
		spec.Port, spec.Seconds, spec.Platform = 8080, 5, p
		def, err := verifyState(app, cfg, spec).Marshal(ctx, llb.Platform(p))
		if err != nil {
			return nil, err
		}
		verified = true
		if _, err := gw.Solve(ctx, gateway.SolveRequest{Definition: def.ToPB(), Evaluate: true}); err != nil {
			return nil, err
		}
		return res, nil
	}, ch)
	if !verified {
		t.Fatalf("the image could not be built; readiness was never checked: %v", err)
	}
	if wantErr && err == nil {
		t.Fatal("an image that cannot start should have failed verification")
	}
	if !wantErr && err != nil {
		t.Fatalf("an image that starts should have passed verification: %v", err)
	}
}
