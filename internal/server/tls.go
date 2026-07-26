package server

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/adrianliechti/donkey/internal/k8s/manifests"
)

// certReadyWait is how long a deploy waits for a fresh certificate. A CA
// issuer answers in a moment; ACME can take longer, and that is fine — the
// app is already running, only https lags behind.
const certReadyWait = 45 * time.Second

// attachCertificate puts the app's certificate on the gateway's https
// listener. The Certificate object itself is applied with the app's other
// objects; this is the one step that has to touch shared state, so it is
// read-modify-write over the single listener every app's certificate hangs on.
func (s *Server) attachCertificate(ctx context.Context, space, app string) error {
	secret := manifests.AppCertName(space, app)
	if _, err := s.k8s.SetGatewayCertificate(ctx, s.cfg.GatewayNamespace, s.cfg.GatewayName, secret, true); err != nil {
		return err
	}
	s.retireLegacyCertificate(ctx, space, app)
	return nil
}

// detachCertificate takes an app's certificate off the listener again (on
// destroy), so a removed app leaves nothing behind.
func (s *Server) detachCertificate(ctx context.Context, space, app string) error {
	secret := manifests.AppCertName(space, app)
	if _, err := s.k8s.SetGatewayCertificate(ctx, s.cfg.GatewayNamespace, s.cfg.GatewayName, secret, false); err != nil {
		return err
	}
	if err := s.k8s.DeleteObject(ctx, "Certificate", s.cfg.GatewayNamespace, secret); err != nil {
		return err
	}
	s.retireLegacyCertificate(ctx, space, app)
	return s.k8s.DeleteObject(ctx, "Secret", s.cfg.GatewayNamespace, secret)
}

// retireLegacyCertificate cleans up the pre-hash certificate name an app may
// have from before names became collision-proof — without this, the old
// Certificate would keep renewing against the issuer forever and its listener
// ref could never be removed. Best effort: a leftover must not fail a deploy.
func (s *Server) retireLegacyCertificate(ctx context.Context, space, app string) {
	legacy := "donkey-tls-" + space + "-" + app
	if legacy == manifests.AppCertName(space, app) {
		return
	}
	_, _ = s.k8s.SetGatewayCertificate(ctx, s.cfg.GatewayNamespace, s.cfg.GatewayName, legacy, false)
	_ = s.k8s.DeleteObject(ctx, "Certificate", s.cfg.GatewayNamespace, legacy)
	_ = s.k8s.DeleteObject(ctx, "Secret", s.cfg.GatewayNamespace, legacy)
}

// routeReadyWait bounds how long a deploy waits for the Gateway to accept a
// protected app's route (including resolving its cross-namespace reference
// into the login gateway).
const routeReadyWait = 30 * time.Second

// waitForRouteReady checks that a protected app's route was accepted and its
// backend — the login gateway — resolved. Unlike a pending certificate this
// is not cosmetic: protection is the route pointing into donkey-gateway, so
// an unresolved route means the app doesn't serve, and the deploy says why
// instead of reporting success.
func (s *Server) waitForRouteReady(ctx context.Context, ns, app string) error {
	deadline := time.Now().Add(routeReadyWait)
	var reason string
	for {
		accepted, detail, err := s.k8s.RouteAccepted(ctx, ns, app)
		switch {
		case err != nil && apierrors.IsNotFound(err):
			reason = "the route hasn't been picked up yet"
		case err != nil:
			reason = err.Error()
		case accepted:
			return nil
		default:
			reason = detail
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"the login-protected route for %s isn't serving (%s) — check `donkey doctor`, or set `public: true` in donkey.yaml if this app is meant to be open",
				app, reason)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

// retractLegacyLogin removes the per-app OIDC objects earlier donkey
// versions rendered (Envoy SecurityPolicy + client-secret Secret). Login
// lives in donkey-gateway now; a leftover policy would keep Envoy running
// its own, second login in front of it.
func (s *Server) retractLegacyLogin(ctx context.Context, ns, app string) error {
	if err := s.k8s.DeleteObject(ctx, "SecurityPolicy", ns, app); err != nil {
		return err
	}
	return s.k8s.DeleteObject(ctx, "Secret", ns, manifests.OIDCSecretName(app))
}

// waitForCertificate reports whether cert-manager finished issuing. It never
// fails a deploy: a certificate that is still pending means https starts
// working a little later, and the user should hear that plainly.
func (s *Server) waitForCertificate(ctx context.Context, space, app, host string) string {
	name := manifests.AppCertName(space, app)
	deadline := time.Now().Add(certReadyWait)
	var lastReason string
	for {
		ready, reason, err := s.k8s.CertificateReady(ctx, s.cfg.GatewayNamespace, name)
		switch {
		case err != nil && apierrors.IsNotFound(err):
			lastReason = "cert-manager hasn't picked it up yet"
		case err != nil:
			return fmt.Sprintf("Couldn't check the certificate for %s: %v", host, err)
		case ready:
			return ""
		default:
			lastReason = reason
		}
		if time.Now().After(deadline) {
			return fmt.Sprintf(
				"The certificate for %s is still being issued (%s) — the app is running; https starts working as soon as %s finishes.",
				host, lastReason, s.cfg.TLSIssuer)
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(2 * time.Second):
		}
	}
}
