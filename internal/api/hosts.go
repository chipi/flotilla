package api

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/chipi/flotilla/internal/hostca"
	"github.com/chipi/flotilla/internal/store"
)

// ADR-0006 §3: a host agent's identity. An operator mints an enrolment token for a host (single use,
// 15 minutes); `flotilla host install` carries it to the host inside the SSH session; the agent sends it
// with a certificate request for the key it made on the host and gets its certificate. Revoking needs
// no access to the host: flotillad, the only verifier, stops accepting the certificate.

func (s *Server) hostRoutes(m *http.ServeMux) {
	m.Handle("GET /v1/hosts", s.auth(ActRead, s.hostsList))
	m.Handle("GET /v1/hosts/{name}/executions", s.auth(ActRead, s.hostExecutions))
	m.Handle("POST /v1/hosts/{name}/enrolment", s.auth(ActHosts, s.hostEnrolment))
	m.Handle("POST /v1/hosts/{name}/revoke", s.auth(ActHosts, s.hostRevoke))
}

var errNoHostCA = problem(503, "this flotillad has no host CA configured: no host can enrol")

// MintEnrolment returns a new enrolment token for host, shown once, and when it expires.
func (s *Server) MintEnrolment(ctx context.Context, c Caller, via, host string) (string, time.Time, error) {
	if err := s.Check(ctx, c, ActHosts); err != nil {
		return "", time.Time{}, err
	}
	if !hostca.HostNameRe.MatchString(host) {
		return "", time.Time{}, problem(400, "a host name is 1 to 63 characters: a-z first, then a-z 0-9 -")
	}
	if s.HostCA == nil {
		return "", time.Time{}, errNoHostCA
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	tok := "fle_" + hex.EncodeToString(b)
	until, err := s.Store.AddEnrolment(ctx, host, HashToken(tok), c.Name, via)
	if err != nil {
		return "", time.Time{}, err
	}
	s.Log.Info("host enrolment token minted", "host", host, "by", c.Name, "until", until)
	return tok, until, nil
}

// EnrolHost spends an enrolment token for host and returns the host's certificate and the intermediate
// (PEM, leaf first), issued for the request's key. The connection the agent enrols over is H6's.
func (s *Server) EnrolHost(ctx context.Context, host, token string, csrDER []byte) ([]byte, error) {
	if s.HostCA == nil {
		return nil, errNoHostCA
	}
	var cert *x509.Certificate
	err := s.Store.Enrol(ctx, host, HashToken(token), func() (string, time.Time, error) {
		c, err := s.HostCA.IssueHost(csrDER, host, time.Now())
		if err != nil {
			return "", time.Time{}, problem(400, "the certificate request: %v", err)
		}
		cert = c
		return hostca.Fingerprint(c), c.NotAfter, nil
	})
	if errors.Is(err, store.ErrEnrolment) {
		s.Log.Warn("host enrolment refused", "host", host)
		return nil, problem(403, "%s", err.Error())
	}
	if err != nil {
		return nil, err
	}
	s.Log.Info("host enrolled", "host", host, "certificate", hostca.Fingerprint(cert), "until", cert.NotAfter)
	return append(hostca.CertPEM(cert), hostca.CertPEM(s.HostCA.Cert)...), nil
}

// AuthenticateHost is flotillad's check of a connecting agent's chain: up to the root that issued the
// intermediate, for client use, and the host's current certificate, not revoked. It returns the host.
func (s *Server) AuthenticateHost(ctx context.Context, chain []*x509.Certificate) (string, error) {
	if s.HostCA == nil || s.HostRoot == nil {
		return "", errNoHostCA
	}
	host, cert, err := hostca.VerifyHost(chain, s.HostRoot, time.Now())
	if err != nil {
		return "", err
	}
	ok, err := s.Store.HostCertCurrent(ctx, host, cert)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", store.ErrNotCurrent
	}
	return host, nil
}

// RenewHost issues an authenticated host's next certificate, over its connection, for the key in csr,
// in place of oldCert, the fingerprint of the certificate it connected with (which must still be
// current). It returns the certificate and the intermediate, PEM.
func (s *Server) RenewHost(ctx context.Context, host, oldCert string, csrDER []byte) ([]byte, error) {
	if s.HostCA == nil {
		return nil, errNoHostCA
	}
	c, err := s.HostCA.IssueHost(csrDER, host, time.Now())
	if err != nil {
		return nil, fmt.Errorf("the certificate request: %w", err)
	}
	if err := s.Store.RenewHost(ctx, host, oldCert, hostca.Fingerprint(c), c.NotAfter); err != nil {
		return nil, err
	}
	s.Log.Info("host certificate renewed", "host", host, "certificate", hostca.Fingerprint(c), "until", c.NotAfter)
	return append(hostca.CertPEM(c), hostca.CertPEM(s.HostCA.Cert)...), nil
}

func (s *Server) RevokeHost(ctx context.Context, c Caller, via, host string) error {
	if err := s.Check(ctx, c, ActHosts); err != nil {
		return err
	}
	err := s.Store.RevokeHost(ctx, host, c.Name, via)
	if errors.Is(err, store.ErrNoSuchHost) {
		return problem(404, "%s", err.Error())
	}
	if err == nil {
		s.Log.Warn("host revoked", "host", host, "by", c.Name)
	}
	return err
}

// --- the HTTP handlers ---

func (s *Server) hostsList(w http.ResponseWriter, r *http.Request, c Caller) {
	hs, err := s.Store.Hosts(r.Context())
	if err != nil {
		writeAction(w, err, 0, nil)
		return
	}
	writeJSON(w, 200, map[string]any{"hosts": hs})
}

// hostExecutions is flotillad's side of an audit (`flotilla host audit`): what it sent the host, what
// came back, and the chain head it last heard.
func (s *Server) hostExecutions(w http.ResponseWriter, r *http.Request, c Caller) {
	name := r.PathValue("name")
	hs, err := s.Store.Hosts(r.Context())
	if err != nil {
		writeAction(w, err, 0, nil)
		return
	}
	head, found := "", false
	for _, h := range hs {
		if h.Name == name {
			head, found = h.ChainHead, true
		}
	}
	if !found {
		writeAction(w, problem(404, "no such host"), 0, nil)
		return
	}
	xs, err := s.Store.HostExecutions(r.Context(), name)
	if err != nil {
		writeAction(w, err, 0, nil)
		return
	}
	writeJSON(w, 200, map[string]any{"host": name, "chain_head": head, "executions": xs})
}

func (s *Server) hostEnrolment(w http.ResponseWriter, r *http.Request, c Caller) {
	tok, until, err := s.MintEnrolment(r.Context(), c, "api", r.PathValue("name"))
	if err != nil {
		writeAction(w, err, 0, nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 201, map[string]string{"host": r.PathValue("name"), "token": tok, "expires_at": until.Format(time.RFC3339),
		"note": "single use; carry it to the host inside the SSH session only; flotilla keeps only its hash"})
}

func (s *Server) hostRevoke(w http.ResponseWriter, r *http.Request, c Caller) {
	err := s.RevokeHost(r.Context(), c, "api", r.PathValue("name"))
	writeAction(w, err, 200, map[string]string{"host": r.PathValue("name"), "revoked_by": c.Name})
}
