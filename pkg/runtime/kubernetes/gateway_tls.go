package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	rt "github.com/adrianliechti/donkey/pkg/runtime"
	"github.com/adrianliechti/donkey/pkg/runtime/kubernetes/manifests"
	"golang.org/x/crypto/acme/autocert"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (g *GatewayAdapter) PrepareGateway(ctx context.Context) error {
	return g.Client.MigrateGatewaySecrets(ctx, g.Namespace)
}

func (g *GatewayAdapter) GatewayCertificateCache() autocert.Cache {
	return &secretCache{client: g.Client, namespace: g.Namespace, name: manifests.GatewayCertsSecret}
}

func (g *GatewayAdapter) GatewayCertificates(ctx context.Context) ([]rt.GatewayCertificate, error) {
	selector := fmt.Sprintf("%s=%s,%s in (%s,%s)", manifests.LabelInstallation, g.Namespace,
		manifests.LabelRole, manifests.RoleEdge, manifests.SystemRole(manifests.GatewayAppName))
	list, err := g.Client.Clientset.CoreV1().Secrets(g.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	var out []rt.GatewayCertificate
	for _, secret := range list.Items {
		if secret.DeletionTimestamp != nil || secret.Labels[manifests.LabelInstallation] != g.Namespace {
			continue
		}
		role := secret.Labels[manifests.LabelRole]
		if role != manifests.RoleEdge && (role != manifests.SystemRole(manifests.GatewayAppName) || secret.Name != manifests.SystemCertificateSecretName) {
			continue
		}
		cert, key := secret.Data["tls.crt"], secret.Data["tls.key"]
		if len(cert) == 0 || len(key) == 0 {
			continue
		}
		out = append(out, rt.GatewayCertificate{Name: secret.Namespace + "/" + secret.Name, Certificate: cert, PrivateKey: key})
	}
	return out, nil
}

func (g *GatewayAdapter) GatewayCA(ctx context.Context, create func() ([]byte, []byte, error)) ([]byte, []byte, error) {
	secrets := g.Client.Clientset.CoreV1().Secrets(g.Namespace)
	sec, err := secrets.Get(ctx, manifests.GatewayCASecret, metav1.GetOptions{})
	if err == nil {
		return sec.Data["ca.crt"], sec.Data["ca.key"], nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, nil, err
	}
	cert, key, err := create()
	if err != nil {
		return nil, nil, err
	}
	labels := manifests.SystemLabels(manifests.GatewayAppName)
	labels[manifests.LabelInstallation], labels[manifests.LabelInstance] = g.Namespace, g.Namespace
	fresh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: manifests.GatewayCASecret, Namespace: g.Namespace, Labels: labels}, Data: map[string][]byte{"ca.crt": cert, "ca.key": key}}
	if _, err := secrets.Create(ctx, fresh, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, nil, err
		}
		sec, err := secrets.Get(ctx, manifests.GatewayCASecret, metav1.GetOptions{})
		if err != nil {
			return nil, nil, err
		}
		return sec.Data["ca.crt"], sec.Data["ca.key"], nil
	}
	return cert, key, nil
}

var _ rt.GatewayBackend = (*GatewayAdapter)(nil)

type secretCache struct {
	client      *Client
	namespace   string
	name        string
	mu          sync.Mutex
	loaded      bool
	entries     map[string][]byte
	lastAttempt time.Time
	lastErr     error
}

// cacheKey maps an autocert key onto a legal Secret data key: autocert uses
// "+" (acme_account+key, <domain>+token), which Secret keys may not contain.
// The replacement token cannot appear in domain names, so keys stay unique.
func cacheKey(key string) string { return strings.ReplaceAll(key, "+", ".plus.") }

// Read once per gateway process. Certificate misses must not turn random
// SNI traffic into API reads. Writes happen only during issuance/renewal.
func (c *secretCache) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		if time.Since(c.lastAttempt) < 5*time.Second {
			return nil, c.lastErr
		}
		c.lastAttempt = time.Now()
		sec, err := c.client.Clientset.CoreV1().Secrets(c.namespace).Get(ctx, c.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			c.entries = map[string][]byte{}
		} else if err != nil {
			c.lastErr = err
			return nil, err
		} else {
			c.entries = sec.Data
		}
		c.loaded = true
	}
	b, ok := c.entries[cacheKey(key)]
	if !ok {
		return nil, autocert.ErrCacheMiss
	}
	return append([]byte(nil), b...), nil
}

func (c *secretCache) Put(ctx context.Context, key string, data []byte) error {
	return c.update(ctx, func(m map[string][]byte) {
		m[cacheKey(key)] = append([]byte(nil), data...)
	})
}

func (c *secretCache) Delete(ctx context.Context, key string) error {
	return c.update(ctx, func(m map[string][]byte) {
		delete(m, cacheKey(key))
	})
}

// update read-modify-writes the Secret, retrying the races two replicas or a
// burst of first-install issuances run into. One Secret caps out near 1 MiB
// (~250 hostnames); past that, certificates still serve from memory but stop
// persisting.
func (c *secretCache) update(ctx context.Context, mutate func(map[string][]byte)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	secrets := c.client.Clientset.CoreV1().Secrets(c.namespace)
	var lastErr error
	for attempt := range 5 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 50 * time.Millisecond):
			}
		}
		sec, err := secrets.Get(ctx, c.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			labels := manifests.SystemLabels(manifests.GatewayAppName)
			labels[manifests.LabelInstallation] = c.namespace
			labels[manifests.LabelInstance] = c.namespace
			fresh := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: c.name, Namespace: c.namespace, Labels: labels},
				Data:       map[string][]byte{},
			}
			mutate(fresh.Data)
			if _, err := secrets.Create(ctx, fresh, metav1.CreateOptions{}); err == nil {
				c.entries, c.loaded = fresh.Data, true
				return nil
			} else if !apierrors.IsAlreadyExists(err) {
				return err
			} else {
				lastErr = err
			}
			continue // somebody else created it first — merge into theirs
		}
		if err != nil {
			return err
		}
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		mutate(sec.Data)
		if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err == nil {
			c.entries, c.loaded = sec.Data, true
			return nil
		} else if !apierrors.IsConflict(err) {
			return err
		} else {
			lastErr = err
		}
	}
	return lastErr
}
