package k8s

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"monobox.ch/donkey/internal/k8s/manifests"
)

var gatewayGVR = schema.GroupVersionResource{
	Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways",
}

// GatewayCertificateRefs lists the secrets the https listener serves. Each app
// has its own certificate there; Envoy picks one per request by SNI.
func (c *Client) GatewayCertificateRefs(ctx context.Context, namespace, name string) ([]string, error) {
	gw, err := c.Dynamic.Resource(gatewayGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	refs, _, err := certRefsOf(gw)
	return refs, err
}

// SetGatewayCertificate adds or removes one app certificate on the https
// listener and reports whether anything changed. Read-modify-write keeps the
// other apps' certificates intact — they are all in one list.
func (c *Client) SetGatewayCertificate(ctx context.Context, namespace, name, secret string, present bool) (bool, error) {
	ri := c.Dynamic.Resource(gatewayGVR).Namespace(namespace)
	gw, err := ri.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, fmt.Errorf("the donkey gateway is missing — run `donkey server install`")
	}
	if err != nil {
		return false, err
	}
	refs, idx, err := certRefsOf(gw)
	if err != nil {
		return false, err
	}
	var next []string
	found := false
	for _, ref := range refs {
		if ref == secret {
			found = true
			if !present {
				continue
			}
		}
		next = append(next, ref)
	}
	if present && !found {
		next = append(next, secret)
	}
	if found == present {
		return false, nil
	}

	listeners, _, err := unstructured.NestedSlice(gw.Object, "spec", "listeners")
	if err != nil {
		return false, err
	}
	entries := make([]any, 0, len(next))
	for _, ref := range next {
		entries = append(entries, map[string]any{"name": ref})
	}
	listener, _ := listeners[idx].(map[string]any)
	tls, ok := listener["tls"].(map[string]any)
	if !ok {
		tls = map[string]any{"mode": "Terminate"}
	}
	tls["certificateRefs"] = entries
	listener["tls"] = tls
	listeners[idx] = listener
	if err := unstructured.SetNestedSlice(gw.Object, listeners, "spec", "listeners"); err != nil {
		return false, err
	}
	if _, err := ri.Update(ctx, gw, metav1.UpdateOptions{FieldManager: fieldManager}); err != nil {
		return false, err
	}
	return true, nil
}

// certRefsOf returns the https listener's certificate secrets and the
// listener's position in the list.
func certRefsOf(gw *unstructured.Unstructured) ([]string, int, error) {
	listeners, _, err := unstructured.NestedSlice(gw.Object, "spec", "listeners")
	if err != nil {
		return nil, 0, err
	}
	for i, item := range listeners {
		listener, ok := item.(map[string]any)
		if !ok || listener["name"] != manifests.HTTPSListenerName {
			continue
		}
		tls, _ := listener["tls"].(map[string]any)
		entries, _ := tls["certificateRefs"].([]any)
		var refs []string
		for _, entry := range entries {
			if ref, ok := entry.(map[string]any); ok {
				if name, ok := ref["name"].(string); ok {
					refs = append(refs, name)
				}
			}
		}
		return refs, i, nil
	}
	return nil, 0, fmt.Errorf("the donkey gateway has no %q listener — run `donkey server install`", manifests.HTTPSListenerName)
}

// CertificateReady reports whether cert-manager finished issuing, and why not
// when it hasn't.
func (c *Client) CertificateReady(ctx context.Context, namespace, name string) (bool, string, error) {
	gvr := schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
	cert, err := c.Dynamic.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, "", err
	}
	conditions, _, err := unstructured.NestedSlice(cert.Object, "status", "conditions")
	if err != nil {
		return false, "", err
	}
	for _, item := range conditions {
		cond, ok := item.(map[string]any)
		if !ok || cond["type"] != "Ready" {
			continue
		}
		message, _ := cond["message"].(string)
		if cond["status"] == "True" {
			return true, message, nil
		}
		if message == "" {
			message, _ = cond["reason"].(string)
		}
		return false, message, nil
	}
	return false, "waiting for cert-manager", nil
}

// CertManagerInstalled reports whether the cert-manager CRDs donkey needs are
// present — a required prerequisite, so `donkey doctor` can say it plainly.
func (c *Client) CertManagerInstalled(ctx context.Context) error {
	gvr := schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}
	_, err := c.Dynamic.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return fmt.Errorf("cert-manager doesn't answer — donkey needs it to issue app certificates: %w", err)
	}
	return nil
}

// IssuerReady checks the configured cert-manager issuer exists and is ready.
// A ClusterIssuer is cluster-scoped; only a plain Issuer lives in a namespace.
func (c *Client) IssuerReady(ctx context.Context, kind, name, namespace string) (bool, string, error) {
	gvr := schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "clusterissuers"}
	ri := c.Dynamic.Resource(gvr)
	var issuers dynamic.ResourceInterface = ri
	if kind == "Issuer" {
		gvr.Resource = "issuers"
		issuers = c.Dynamic.Resource(gvr).Namespace(namespace)
	}
	issuer, err := issuers.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, "", err
	}
	conditions, _, err := unstructured.NestedSlice(issuer.Object, "status", "conditions")
	if err != nil {
		return false, "", err
	}
	for _, item := range conditions {
		cond, ok := item.(map[string]any)
		if !ok || cond["type"] != "Ready" {
			continue
		}
		message, _ := cond["message"].(string)
		return cond["status"] == "True", message, nil
	}
	return false, "no readiness reported yet", nil
}
