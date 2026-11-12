package docker

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
)

var readinessClient = &http.Client{
	Timeout: 2 * time.Second,
	// Cluster/container traffic must not go through an operator's HTTP proxy.
	Transport:     &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func (r *Runtime) probeContainer(ctx context.Context, info container.InspectResponse) error {
	if info.Config == nil {
		return fmt.Errorf("the container has no configuration")
	}
	if info.State != nil && info.State.Health != nil && info.State.Health.Status != "healthy" {
		return fmt.Errorf("the image's health check is %s", info.State.Health.Status)
	}
	port, _ := strconv.Atoi(info.Config.Labels[labelPort])
	if port <= 0 {
		// Infrastructure helpers do not always carry app endpoint labels.
		if info.Config.Labels[labelRole] != roleWorkload {
			return nil
		}
		return fmt.Errorf("the app has no configured listening port")
	}
	host := strings.TrimPrefix(info.Name, "/")
	if host == "" {
		return fmt.Errorf("the container has no address")
	}
	return probeEndpoint(ctx, net.JoinHostPort(host, strconv.Itoa(port)), info.Config.Labels[labelHost], info.Config.Labels[labelHealthPath])
}

// Probe from outside the app: a loopback-only listener must not pass. A
// configured HTTP route is tested with the same Host that visitors send.
func probeEndpoint(ctx context.Context, address, host, healthPath string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if healthPath == "" {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		if err != nil {
			return fmt.Errorf("the app is not listening at %s: %w", address, err)
		}
		return conn.Close()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+healthPath, nil)
	if err != nil {
		return err
	}
	if host != "" {
		req.Host = host
	}
	res, err := readinessClient.Do(req)
	if err != nil {
		return fmt.Errorf("health route %s: %w", healthPath, err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode < 200 || res.StatusCode >= 400 {
		return fmt.Errorf("health route %s answered HTTP %d", healthPath, res.StatusCode)
	}
	return nil
}
