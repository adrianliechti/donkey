package addons

import (
	"context"
	"fmt"
	"sync"

	"github.com/adrianliechti/donkey/internal/config"
)

// CheckType prevents a named service from being reused as a different provider.
// Bindings predating type metadata still carry a recognizable protocol.
func (m Metadata) CheckType(typ string) error {
	current := m.Type
	if current == "" {
		current = NormalizeProtocol(m.Protocol)
	}
	if current != "" && config.NormalizeAddonType(current) != config.NormalizeAddonType(typ) {
		return fmt.Errorf("this service already uses %s; remove it before changing its type to %s", current, typ)
	}
	return nil
}

// CheckKeepData allows retaining credentials only for services with stored data. Plugin
// retention is checked by their deprovisioning path, including orphan removal.
func (m Metadata) CheckKeepData(ctx context.Context, requested bool) error {
	if !requested {
		return nil
	}
	typ := m.Type
	if typ == "" {
		typ = NormalizeProtocol(m.Protocol)
	}
	typ = config.NormalizeAddonType(typ)
	a := builtins[typ]
	if a == nil {
		a = proxyBuiltin(typ)
	}
	if a == nil {
		return nil
	}
	desc, err := a.Describe(ctx)
	if err != nil {
		return err
	}
	if !desc.GetCapabilities().GetRetainData() {
		return fmt.Errorf("this service has no stored data; remove without --keep-data to revoke access")
	}
	return nil
}

// InstanceLocks serialize service creation/removal across apps of one space.
// App locks alone cannot protect credentials belonging to a shared service.
type InstanceLocks struct{ locks sync.Map }

func (l *InstanceLocks) Lock(space, name string) func() {
	value, _ := l.locks.LoadOrStore(space+"/"+name, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
