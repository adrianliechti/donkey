package machine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"time"
)

// A machine that nobody uses stops. It is somebody's desk, not a service:
// worth its CPU and memory while its owner is at it or its agent is working,
// and a start brings everything back, because the disk stays.
//
// "In use" is what the front door can see without knowing the agent's
// protocol, and it is deliberately generous:
//
//   - a browser is talking to the machine — a request through the gateway,
//     or bytes on a connection that stays open (a terminal, the agent's
//     stream), in either direction;
//   - something in the machine is talking to its model — which is how an
//     agent that was left to finish a long task keeps its machine, with no
//     browser anywhere.
//
// The control plane's own questions do not count: the console asks a running
// machine for its folders every few seconds for as long as its page is open,
// whoever is or is not looking at it.

// Activity is when the machine was last used.
type Activity struct{ last atomic.Int64 }

// Mark says the machine is being used right now.
func (a *Activity) Mark() { a.last.Store(time.Now().UnixNano()) }

// Since is how long the machine has gone unused.
func (a *Activity) Since() time.Duration { return time.Since(time.Unix(0, a.last.Load())) }

// usedStream marks the machine used whenever bytes pass through a connection
// the gateway upgraded — the only sign of life a terminal gives.
type usedStream struct {
	io.ReadWriteCloser
	used func()
}

func (s usedStream) Read(p []byte) (int, error) {
	n, err := s.ReadWriteCloser.Read(p)
	if n > 0 {
		s.used()
	}
	return n, err
}

func (s usedStream) Write(p []byte) (int, error) {
	n, err := s.ReadWriteCloser.Write(p)
	if n > 0 {
		s.used()
	}
	return n, err
}

// usedBody marks the machine used for as long as an answer keeps arriving.
type usedBody struct {
	io.ReadCloser
	used func()
}

func (b usedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.used()
	}
	return n, err
}

// modelRelay stands between the machine and its model for one reason: to see
// that the model is being asked. It changes nothing on the way — the token
// the machine was given is still what donkey-ai checks.
func modelRelay(target *url.URL, used func()) http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			used()
			pr.Out.URL.Scheme, pr.Out.URL.Host = target.Scheme, target.Host
			pr.Out.Host = target.Host
		},
		ModifyResponse: func(res *http.Response) error {
			res.Body = usedBody{res.Body, used}
			return nil
		},
		// Model answers stream.
		FlushInterval: -1,
	}
}

// watchIdle asks for the machine to be stopped once it has gone unused for
// timeout. The machine cannot stop itself — a container that exits is started
// again — so it asks the control plane, and keeps asking while the answer is
// no: a sign-in that has ended, an API that is restarting.
func watchIdle(ctx context.Context, activity *Activity, timeout time.Duration, stop func(context.Context) error, log io.Writer) {
	ticker := time.NewTicker(min(timeout/4, 30*time.Second))
	defer ticker.Stop()
	var notBefore time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if activity.Since() < timeout || now.Before(notBefore) {
				continue
			}
			fmt.Fprintf(log, "donkey machine: unused for %s — asking to be stopped. The disk is kept.\n", timeout)
			if err := stop(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				fmt.Fprintf(log, "donkey machine: it could not be stopped: %v\n", err)
				notBefore = now.Add(time.Minute)
				continue
			}
			// The runtime takes it from here. Should the machine still be
			// running in a while, it asks again.
			notBefore = now.Add(5 * time.Minute)
		}
	}
}
