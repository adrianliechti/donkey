package docker

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/adrianliechti/donkey/internal/config"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"sigs.k8s.io/yaml"
)

// Keep the original container and app records until the candidate is ready.
// Reusing that container restores its exact image, command and environment;
// application data on volumes is never copied, deleted or rolled back here.
type appRecovery struct {
	r               *Runtime
	space, app, id  string
	running, parked bool
	records         []savedRecord
}
type savedRecord struct {
	path []string
	data []byte
}

func (r *Runtime) prepareRecovery(ctx context.Context, space string, app *config.AppConfig) (*appRecovery, error) {
	name := ctrName(space, app.Name)
	info, err := r.cli.ContainerInspect(ctx, name)
	if cerrdefs.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.ContainerJSONBase == nil || info.Config == nil || info.ID == "" {
		return nil, fmt.Errorf("cannot preserve container %s without its identity", name)
	}
	if err := requireIdentity("container", name, info.Config.Labels, appLabels(space, app.Name, roleWorkload)); err != nil {
		return nil, err
	}
	recovery := &appRecovery{r: r, space: space, app: app.Name, id: info.ID, running: info.State != nil && info.State.Running}
	paths := [][]string{{"env", space, app.Name}, {"meta", space, app.Name}, {"deploy", space, app.Name}, {"routes", space, app.Name}}
	bindings, err := r.state.listBindings(space, app.Name)
	if err != nil {
		return nil, err
	}
	for _, addon := range app.Addons {
		bindings = append(bindings, addon.Name)
	}
	for _, binding := range bindings {
		paths = append(paths, []string{"bindings", space, bindingName(app.Name, binding)})
	}
	routes, err := r.state.listRoutes(space)
	if err != nil {
		return nil, err
	}
	for _, route := range routes {
		if strings.HasPrefix(route, app.Name+".on.") {
			paths = append(paths, []string{"routes", space, route})
		}
	}
	for _, route := range app.Routes {
		paths = append(paths, []string{"routes", space, pathRouteName(app.Name, route.App, route.Path)})
	}
	seen := map[string]bool{}
	for _, p := range paths {
		key := strings.Join(p, "/")
		if seen[key] {
			continue
		}
		seen[key] = true
		data, err := readRecordFile(r.state.path(p...))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		recovery.records = append(recovery.records, savedRecord{path: p, data: data})
	}
	return recovery, nil
}

func (a *appRecovery) park(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if err := a.r.cli.ContainerStop(ctx, a.id, container.StopOptions{}); err != nil {
		return err
	}
	// Mark before renaming so a rename failure still restarts the old app.
	a.parked = true
	return a.r.cli.ContainerRename(ctx, a.id, ctrName(a.space, a.app)+".previous-"+strings.ToLower(rand.Text()[:12]))
}

func (a *appRecovery) restore(ctx context.Context) error {
	if a == nil {
		return nil
	}
	var result error
	for _, record := range a.records {
		var err error
		if record.data == nil {
			err = a.r.state.remove(record.path...)
		} else {
			// The saved bytes may be a legacy JSON record; either way the
			// record goes back as YAML.
			var data []byte
			if data, err = yaml.YAMLToJSON(record.data); err == nil {
				err = a.r.state.writeRecord(json.RawMessage(data), record.path...)
			}
		}
		result = errors.Join(result, err)
	}
	if !a.parked {
		return result
	}
	name := ctrName(a.space, a.app)
	info, err := a.r.cli.ContainerInspect(ctx, name)
	currentID := ""
	if err == nil && info.ContainerJSONBase != nil {
		currentID = info.ID
	}
	if err == nil && currentID != a.id {
		if err := a.r.removeOwnedContainer(ctx, name, appLabels(a.space, a.app, roleWorkload)); err != nil {
			return errors.Join(result, err)
		}
	} else if err != nil && !cerrdefs.IsNotFound(err) {
		return errors.Join(result, err)
	}
	if currentID != a.id {
		if err := a.r.cli.ContainerRename(ctx, a.id, name); err != nil {
			return errors.Join(result, err)
		}
	}
	if a.running {
		result = errors.Join(result, a.r.cli.ContainerStart(ctx, a.id, container.StartOptions{}))
	}
	return result
}

func (a *appRecovery) discard(ctx context.Context) error {
	if a == nil || !a.parked {
		return nil
	}
	return a.r.cli.ContainerRemove(ctx, a.id, container.RemoveOptions{})
}

func recoveryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}
