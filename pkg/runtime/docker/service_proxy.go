package docker

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/addons"
	"github.com/adrianliechti/donkey/internal/config"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
)

type proxyNetwork struct {
	Updated time.Time `json:"updated"`
	CIDRs   []string  `json:"cidrs"`
}

func (r *Runtime) recordProxyNetwork(space string, ipam network.IPAM) error {
	record := proxyNetwork{Updated: time.Now()}
	for _, subnet := range ipam.Config {
		if prefix, err := netip.ParsePrefix(subnet.Subnet); err == nil {
			record.CIDRs = append(record.CIDRs, prefix.Masked().String())
		}
	}
	return r.state.writeJSON(record, "proxy", "networks", space+".json")
}

// The server publishes owned network ranges; the shared proxy reads this
// snapshot without a Docker socket. App IP reuse stays within the space's
// subnet, rather than inheriting a cached individual container identity.
func (r *Runtime) ReconcileProxyNetworks(ctx context.Context) {
	for {
		if err := r.refreshProxyNetworks(ctx); err != nil && ctx.Err() == nil {
			log.Printf("couldn't refresh proxy space networks: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func (r *Runtime) refreshProxyNetworks(ctx context.Context) error {
	networks, err := r.cli.NetworkList(ctx, network.ListOptions{Filters: filters.NewArgs(filters.Arg("label", labelManaged+"=true"), filters.Arg("label", labelInstallation+"="+dockerInstallation), filters.Arg("label", labelRole+"="+roleSpace))})
	if err != nil {
		return err
	}
	current := map[string]bool{}
	for _, nw := range networks {
		space := nw.Labels[labelSpace]
		if config.ValidateSpaceName(space) != nil || !sameIdentity(nw.Labels, resourceLabels(space, roleSpace)) {
			continue
		}
		if err := r.recordProxyNetwork(space, nw.IPAM); err != nil {
			return err
		}
		current[space+".json"] = true
	}
	entries, err := os.ReadDir(r.state.path("proxy", "networks"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") && !current[entry.Name()] {
			if err := r.state.remove("proxy", "networks", entry.Name()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Runtime) ServiceProxyAccess(ctx context.Context, sourceIP, offering string) (bool, error) {
	ip, err := netip.ParseAddr(sourceIP)
	if err != nil {
		return false, nil
	}
	ip = ip.Unmap()
	entries, err := os.ReadDir(r.state.path("proxy", "networks"))
	if err != nil {
		return false, err
	}
	space := ""
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || config.ValidateSpaceName(name) != nil {
			continue
		}
		var record proxyNetwork
		found, err := r.state.readJSON(&record, "proxy", "networks", entry.Name())
		if err != nil {
			return false, err
		}
		if !found {
			continue
		}
		for _, raw := range record.CIDRs {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil {
				return false, err
			}
			if prefix.Contains(ip) {
				if time.Since(record.Updated) > 30*time.Second {
					return false, fmt.Errorf("proxy network identity is stale")
				}
				if space != "" && space != name {
					return false, nil
				}
				space = name
			}
		}
	}
	if space == "" {
		return false, nil
	}
	return r.ServiceProxySpaceAccess(ctx, space, offering)
}

func (r *Runtime) ServiceProxySpaceAccess(ctx context.Context, space, offering string) (bool, error) {
	if config.ValidateSpaceName(space) != nil {
		return false, nil
	}
	names, err := r.state.listBindings(space, sharedOwner)
	if err != nil {
		return false, err
	}
	for _, name := range names {
		binding, err := r.state.binding(space, sharedOwner, name)
		if err != nil {
			return false, err
		}
		if addons.IsProxyBinding(r.serviceProxyURL(), offering, binding) {
			return true, nil
		}
	}
	return false, nil
}
