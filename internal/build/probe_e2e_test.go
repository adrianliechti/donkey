//go:build e2e

package build

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/platforms"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
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
		wantErr bool
	}{
		"an app that listens passes": {
			`CMD ["node", "-e", "require('http').createServer((_,r)=>r.end('ok')).listen(8080)"]`, false,
		},
		"an app that exits fails": {
			`CMD ["node", "-e", "process.exit(3)"]`, true,
		},
	} {
		t.Run(name, func(t *testing.T) { runProbe(t, addr, p, tc.cmd, tc.wantErr) })
	}
}

func runProbe(t *testing.T, addr string, p ocispecs.Platform, cmd string, wantErr bool) {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"),
		[]byte("FROM node:22-alpine\nWORKDIR /app\n"+cmd+"\n"), 0o644)
	fs, err := fsutil.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}

	c, err := bkclient.New(context.Background(), addr)
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
	_, err = c.Build(context.Background(), opt, "probe", func(ctx context.Context, gw gateway.Client) (*gateway.Result, error) {
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
		cfg, err := imageConfigOf(res.Metadata)
		if err != nil {
			return nil, err
		}
		app, err := ref.ToState()
		if err != nil {
			return nil, err
		}
		def, err := verifyState(app, cfg, VerifySpec{Port: 8080, Seconds: 20, Platform: p}).Marshal(ctx, llb.Platform(p))
		if err != nil {
			return nil, err
		}
		if _, err := gw.Solve(ctx, gateway.SolveRequest{Definition: def.ToPB(), Evaluate: true}); err != nil {
			return nil, err
		}
		return res, nil
	}, ch)
	if wantErr && err == nil {
		t.Fatal("an image that cannot start should have failed verification")
	}
	if !wantErr && err != nil {
		t.Fatalf("an image that starts should have passed verification: %v", err)
	}
}
