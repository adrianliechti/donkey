package server

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"monobox.ch/donkey/internal/k8s/manifests"
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
	_, err := s.k8s.SetGatewayCertificate(ctx, s.cfg.GatewayNamespace, s.cfg.GatewayName, secret, true)
	return err
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
	return s.k8s.DeleteObject(ctx, "Secret", s.cfg.GatewayNamespace, secret)
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
