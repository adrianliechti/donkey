package docker

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-units"

	"github.com/adrianliechti/donkey/internal/config"
	rt "github.com/adrianliechti/donkey/pkg/runtime"
)

// Personal machines on Docker. A machine is a container, a named volume for
// its disk and a network of its own:
//
//	container  donkey.platform.<id>        role machine
//	volume     donkey.platform.<id>.data   role machine-storage
//	network    donkey.platform.<id>        role machine-network
//	route      routes/platform/<id>.yaml   (read by the gateway)
//	record     machines/<id>.yaml          (its environment, credentials included)
//
// "platform" is a name no space may take, so none of these can ever be an
// app's. The network is the isolation: a machine shares it with the gateway
// (which proxies to it), donkey-server (the API its CLI calls) and donkey-ai
// (its model) and with nothing else — no app, no other machine.
const (
	labelMachine = "local.donkey.machine"
	// labelSignInExpires is when the sign-in the machine holds runs out.
	labelSignInExpires = "local.donkey.sign-in-expires"

	roleMachine        = "machine"
	roleMachineStorage = "machine-storage"
	roleMachineNetwork = "machine-network"
	roleMachineInit    = "machine-init"

	machineRecords = "machines"
)

var _ rt.MachineProvider = (*Runtime)(nil)

// machines is the runtime's machine half. A type of its own, not more
// methods on Runtime: Status and Logs already mean an app's there.
type machines struct{ r *Runtime }

func (r *Runtime) Machines() rt.Machines { return machines{r} }

func machineName(id string) string { return ctrName(config.MachineSpace, id) }

func machineLabels(id, role string) map[string]string {
	labels := SystemLabels(role)
	labels[labelMachine] = id
	return labels
}

// machineRecord is what a running machine was started with. It holds the
// machine's credentials, like a binding record holds a service's, and is
// removed when the machine stops — which is what withdraws them.
type machineRecord struct {
	Owner string            `json:"owner"`
	Image string            `json:"image"`
	Host  string            `json:"host"`
	Env   map[string]string `json:"env,omitempty"`
}

// machineContainers are donkey's own containers a machine talks to.
func (r *Runtime) machineContainers() []string {
	var names []string
	for _, name := range []string{r.opts.GatewayContainer, r.opts.ServerContainer, r.opts.AIContainer} {
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func (r *Runtime) machineAPIURL() string {
	return fmt.Sprintf("http://%s:8088", r.opts.ServerContainer)
}

func (m machines) Status(ctx context.Context, id string) (*rt.MachineStatus, error) {
	r := m.r
	if !config.ValidMachineID(id) {
		return nil, fmt.Errorf("%q is not a machine", id)
	}
	st := &rt.MachineStatus{State: rt.MachineAbsent}
	name := machineName(id)
	info, err := r.cli.ContainerInspect(ctx, name)
	if cerrdefs.IsNotFound(err) {
		vol, err := r.cli.VolumeInspect(ctx, name+".data")
		if err == nil && sameIdentity(vol.Labels, machineLabels(id, roleMachineStorage)) {
			st.State = rt.MachineStopped
		}
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Config == nil {
		return nil, fmt.Errorf("the Docker container %s has no inspectable configuration", name)
	}
	if err := requireIdentity("container", name, info.Config.Labels, machineLabels(id, roleMachine)); err != nil {
		return nil, err
	}
	st.Image = r.pushRef(info.Config.Labels[labelImage])
	st.Host = info.Config.Labels[labelHost]
	if t, err := time.Parse(time.RFC3339, info.Config.Labels[labelSignInExpires]); err == nil {
		st.SignInExpires = t
	}
	st.Restarts = int32(info.RestartCount)
	st.State = rt.MachineStarting
	state := info.State
	if state == nil {
		return st, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, state.StartedAt); err == nil && t.Unix() > 0 {
		st.StartedAt = t
	}
	switch {
	case state.OOMKilled, state.Status == "exited", state.Status == "dead", state.Restarting:
		st.State = rt.MachineFailed
		st.Message = containerSummary(info)
		if tail := strings.TrimSpace(r.logsTail(ctx, name, 20)); tail != "" {
			st.Message += "\n" + tail
		}
	case state.Running:
		// Running is not serving: the front door only answers once the agent
		// behind it does.
		address := net.JoinHostPort(name, strconv.Itoa(config.MachinePort))
		if err := probeEndpoint(ctx, address, "", rt.MachineHealthPath); err == nil {
			st.State = rt.MachineRunning
		}
	}
	return st, nil
}

func (m machines) Start(ctx context.Context, spec rt.MachineSpec) error {
	r := m.r
	if !config.ValidMachineID(spec.ID) {
		return fmt.Errorf("%q is not a machine", spec.ID)
	}
	name := machineName(spec.ID)
	if err := r.ensureMachineNetwork(ctx, spec.ID); err != nil {
		return err
	}
	volume := name + ".data"
	if err := r.ensureOwnedVolume(ctx, volume, machineLabels(spec.ID, roleMachineStorage)); err != nil {
		return err
	}
	if err := r.initMachineVolume(ctx, spec.ID, volume); err != nil {
		return err
	}
	image := r.pullRef(spec.Image)
	env := rt.MachineEnv(spec, r.machineAPIURL(), r.aiProxyURL())
	// The record goes first: donkey-ai accepts the machine's model token from
	// it, so it has to be there by the time the agent makes its first call.
	record := machineRecord{Owner: spec.Owner, Image: image, Host: spec.Host, Env: env}
	if err := r.state.writeRecord(record, machineRecords, spec.ID); err != nil {
		return err
	}
	labels := machineLabels(spec.ID, roleMachine)
	labels[labelImage] = image
	labels[labelHost] = spec.Host
	if !spec.SignInExpires.IsZero() {
		labels[labelSignInExpires] = spec.SignInExpires.UTC().Format(time.RFC3339)
	}
	cfg := runConfig{
		Image:   image,
		Env:     env,
		Labels:  labels,
		Network: name,
		Volumes: map[string]volumeMount{config.MachineDataPath: {Volume: volume}},
		// Whatever the image says, a machine runs as its own unprivileged
		// user, cannot become anyone else and holds no capability.
		User:     fmt.Sprintf("%d:%d", config.MachineUser, config.MachineUser),
		Hardened: true,
	}
	settings := spec.Settings
	if cpus, err := strconv.ParseFloat(strings.TrimSuffix(settings.CPULimit(), "m"), 64); err == nil && cpus > 0 {
		if strings.HasSuffix(settings.CPULimit(), "m") {
			cpus /= 1000
		}
		cfg.NanoCPUs = int64(cpus * 1e9)
	}
	if memory, err := units.RAMInBytes(strings.TrimSuffix(settings.MemoryLimit(), "i")); err == nil && memory > 0 {
		cfg.Memory = memory
	}
	if err := r.replaceContainer(ctx, name, cfg); err != nil {
		return err
	}
	// The route goes last: the gateway only learns the address once there is
	// something behind it.
	backend := fmt.Sprintf("http://%s:%d", name, config.MachinePort)
	return r.state.setRoute(config.MachineSpace, spec.ID, config.MachineRoute(spec.ID, spec.Owner, spec.Host, backend))
}

func (m machines) Stop(ctx context.Context, id string) error {
	r := m.r
	if !config.ValidMachineID(id) {
		return fmt.Errorf("%q is not a machine", id)
	}
	name := machineName(id)
	// Address and credentials first: the gateway stops sending visitors and
	// donkey-ai stops honouring the machine's token before the container goes.
	if err := r.state.removeRoute(config.MachineSpace, id); err != nil {
		return err
	}
	if err := r.state.remove(machineRecords, id); err != nil {
		return err
	}
	if err := r.removeOwnedContainer(ctx, name, machineLabels(id, roleMachine)); err != nil {
		return err
	}
	return r.removeMachineNetwork(ctx, id)
}

func (m machines) Delete(ctx context.Context, id string) error {
	r := m.r
	if err := m.Stop(ctx, id); err != nil {
		return err
	}
	return r.removeOwnedVolume(ctx, machineName(id)+".data", machineLabels(id, roleMachineStorage))
}

func (m machines) Logs(ctx context.Context, id string, opts rt.LogOptions, out io.Writer) error {
	r := m.r
	if !config.ValidMachineID(id) {
		return fmt.Errorf("%q is not a machine", id)
	}
	name := machineName(id)
	info, err := r.cli.ContainerInspect(ctx, name)
	if cerrdefs.IsNotFound(err) {
		return fmt.Errorf("the machine isn't running")
	}
	if err != nil {
		return err
	}
	if info.Config == nil {
		return fmt.Errorf("the Docker container %s has no inspectable configuration", name)
	}
	if err := requireIdentity("container", name, info.Config.Labels, machineLabels(id, roleMachine)); err != nil {
		return err
	}
	lo := container.LogsOptions{ShowStdout: true, ShowStderr: true, Follow: opts.Follow, Tail: strconv.FormatInt(opts.TailLines, 10)}
	if opts.Since > 0 {
		lo.Since = time.Now().Add(-opts.Since).Format(time.RFC3339)
	}
	rc, err := r.cli.ContainerLogs(ctx, name, lo)
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = stdcopy.StdCopy(out, out, rc)
	if ctx.Err() != nil {
		return nil // a cancelled follow is a normal hang-up
	}
	return err
}

// machineAITokens are the model tokens of the machines that are running.
func (r *Runtime) machineAITokens() ([]string, error) {
	ids, err := r.state.records(machineRecords)
	if err != nil {
		return nil, err
	}
	var tokens []string
	for _, id := range ids {
		var record machineRecord
		found, err := r.state.readRecord(&record, machineRecords, id)
		if err != nil {
			return nil, err
		}
		if !found || !config.ValidMachineID(id) {
			continue
		}
		// Only a token issued for this installation's endpoint counts, the
		// same rule a space's binding follows.
		if token := record.Env[rt.MachineAIKey]; token != "" && record.Env["OPENAI_BASE_URL"] == r.aiProxyURL() {
			tokens = append(tokens, token)
		}
	}
	return tokens, nil
}

// ensureMachineNetwork creates the machine's network and puts the gateway,
// the API and the AI endpoint on it.
func (r *Runtime) ensureMachineNetwork(ctx context.Context, id string) error {
	name := machineName(id)
	labels := machineLabels(id, roleMachineNetwork)
	nw, err := r.cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if cerrdefs.IsNotFound(err) {
		if _, err := r.cli.NetworkCreate(ctx, name, network.CreateOptions{Labels: labels}); err != nil {
			return fmt.Errorf("creating the machine's network: %w", err)
		}
	} else if err != nil {
		return err
	} else if err := requireIdentity("network", name, nw.Labels, labels); err != nil {
		return err
	}
	own, err := ownedSystemContainers(ctx, r.cli, r.machineContainers()...)
	if err != nil {
		return err
	}
	for _, containerName := range own {
		err := r.cli.NetworkConnect(ctx, name, containerName, nil)
		if err != nil && !strings.Contains(err.Error(), "already exists") && !cerrdefs.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *Runtime) removeMachineNetwork(ctx context.Context, id string) error {
	name := machineName(id)
	nw, err := r.cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := requireIdentity("network", name, nw.Labels, machineLabels(id, roleMachineNetwork)); err != nil {
		return err
	}
	// A network with endpoints cannot be removed; donkey's own containers are
	// the only ones left on it.
	for _, endpoint := range nw.Containers {
		if err := r.cli.NetworkDisconnect(ctx, nw.ID, endpoint.Name, true); err != nil && !cerrdefs.IsNotFound(err) {
			return err
		}
	}
	err = r.cli.NetworkRemove(ctx, nw.ID)
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// attachMachineNetworks re-attaches donkey's containers to the networks of
// the machines that exist — the machine half of AttachOwnContainers.
func (r *Runtime) attachMachineNetworks(ctx context.Context) error {
	own, err := ownedSystemContainers(ctx, r.cli, r.machineContainers()...)
	if err != nil {
		return err
	}
	nets, err := r.cli.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", labelManaged+"=true"),
			filters.Arg("label", labelInstallation+"="+dockerInstallation),
			filters.Arg("label", labelRole+"="+roleMachineNetwork),
		),
	})
	if err != nil {
		return err
	}
	for _, nw := range nets {
		id := nw.Labels[labelMachine]
		if !config.ValidMachineID(id) || !sameIdentity(nw.Labels, machineLabels(id, roleMachineNetwork)) {
			continue
		}
		for _, name := range own {
			err := r.cli.NetworkConnect(ctx, nw.Name, name, nil)
			if err != nil && !strings.Contains(err.Error(), "already exists") && !cerrdefs.IsNotFound(err) {
				log.Printf("couldn't attach %s to the network of machine %s: %v", name, id, err)
			}
		}
	}
	return nil
}

// initMachineVolume hands a fresh disk to the machine's user. A new volume
// belongs to root, and the machine runs as somebody who is not.
func (r *Runtime) initMachineVolume(ctx context.Context, id, volume string) error {
	image := r.filesImage()
	if err := r.pullImage(ctx, image); err != nil {
		return err
	}
	labels := machineLabels(id, roleMachineInit)
	name := volume + ".init"
	if err := r.removeOwnedContainer(ctx, name, labels); err != nil {
		return err
	}
	owner := strconv.FormatInt(config.MachineUser, 10)
	if _, err := r.cli.ContainerCreate(ctx, &container.Config{
		Image:      image,
		Entrypoint: []string{"sh", "-c", "chown " + owner + ":" + owner + " " + filesMount + " && chmod 750 " + filesMount},
		Labels:     labels,
	}, &container.HostConfig{
		Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: volume, Target: filesMount}},
	}, nil, nil, name); err != nil {
		return err
	}
	defer func() { _ = r.removeOwnedContainer(context.WithoutCancel(ctx), name, labels) }()
	if err := r.cli.ContainerStart(ctx, name, container.StartOptions{}); err != nil {
		return err
	}
	okC, errC := r.cli.ContainerWait(ctx, name, container.WaitConditionNotRunning)
	select {
	case status := <-okC:
		if status.StatusCode != 0 {
			return fmt.Errorf("preparing the machine's disk failed (exit code %d)", status.StatusCode)
		}
		return nil
	case err := <-errC:
		return err
	}
}
