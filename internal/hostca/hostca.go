// Package hostca is the host agents' certificate authority (ADR-0006 §3 and §11, security T13): an
// offline root kept by the operator, an intermediate in flotillad's secrets volume, and the certificates
// the intermediate issues, to a host agent (its client certificate, 30 days, named for the host) and to
// flotillad (its server certificate). Ed25519 throughout, the standard library only.
//
// The agent pins the root, so the intermediate can rotate. flotillad verifies every connecting agent
// against the root (VerifyHost) and then against its own hosts table, which is where revocation lives:
// it is the only verifier, so there is no CRL or OCSP.
package hostca

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"time"
)

// Validity of each kind of certificate.
const (
	RootValidity         = 10 * 365 * 24 * time.Hour
	IntermediateValidity = 3 * 365 * 24 * time.Hour
	HostValidity         = 30 * 24 * time.Hour
	ServerValidity       = 90 * 24 * time.Hour
	backdate             = time.Minute // a host's clock a little behind flotillad's still accepts it
)

// HostNameRe is a host's name, the same rule the host's own config uses (internal/hostcfg).
var HostNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// CA is a certificate with its key: the root (on the operator's machine) or the intermediate.
type CA struct {
	Cert *x509.Certificate
	Key  ed25519.PrivateKey
}

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func issue(tmpl *x509.Certificate, parent *CA, pub ed25519.PublicKey, signer ed25519.PrivateKey) (*x509.Certificate, error) {
	s, err := serial()
	if err != nil {
		return nil, err
	}
	tmpl.SerialNumber = s
	parentCert := tmpl
	if parent != nil {
		parentCert = parent.Cert
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parentCert, pub, signer)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

func newCA(cn string, parent *CA, validity time.Duration, now time.Time) (*CA, error) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	signer := key
	if parent != nil {
		tmpl.MaxPathLenZero = true // the intermediate signs host and server certificates, never another CA
		signer = parent.Key
		if tmpl.NotAfter.After(parent.Cert.NotAfter) {
			return nil, fmt.Errorf("the root expires %s, before the intermediate would", parent.Cert.NotAfter.Format(time.DateOnly))
		}
	}
	c, err := issue(tmpl, parent, pub, signer)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: c, Key: key}, nil
}

// NewRoot makes the offline root.
func NewRoot(now time.Time) (*CA, error) { return newCA("flotilla root", nil, RootValidity, now) }

// NewIntermediate makes an intermediate under root.
func NewIntermediate(root *CA, now time.Time) (*CA, error) {
	return newCA("flotilla intermediate", root, IntermediateValidity, now)
}

// IssueHost issues a host agent's client certificate for the public key in its certificate request.
// The request must be signed by its key (the agent holds it), Ed25519, and name exactly this host.
func (ca *CA) IssueHost(csrDER []byte, host string, now time.Time) (*x509.Certificate, error) {
	if !HostNameRe.MatchString(host) {
		return nil, fmt.Errorf("host name %q", host)
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("the request's signature: %w", err)
	}
	pub, ok := csr.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("the request's key is not Ed25519")
	}
	if csr.Subject.CommonName != host {
		return nil, fmt.Errorf("the request names %q, not %q", csr.Subject.CommonName, host)
	}
	return issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: host},
		NotBefore:   now.Add(-backdate),
		NotAfter:    now.Add(HostValidity),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, ca, pub, ca.Key)
}

// IssueServer issues flotillad's server certificate for the host agents' endpoint.
func (ca *CA) IssueServer(pub ed25519.PublicKey, names []string, now time.Time) (*x509.Certificate, error) {
	if len(names) == 0 {
		return nil, errors.New("a server certificate needs a name")
	}
	return issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: names[0]},
		DNSNames:    names,
		NotBefore:   now.Add(-backdate),
		NotAfter:    now.Add(ServerValidity),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca, pub, ca.Key)
}

// HostRequest is the agent's certificate request for its own key.
func HostRequest(key ed25519.PrivateKey, host string) ([]byte, error) {
	return x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: host}}, key)
}

// VerifyHost checks a connecting agent's chain (leaf first, as TLS presents it) up to root, at now, for
// client authentication, and returns the host it names and the certificate's Fingerprint. Whether that
// certificate is the host's current one, and not revoked, is the hosts table's to say.
func VerifyHost(chain []*x509.Certificate, root *x509.Certificate, now time.Time) (host, fingerprint string, err error) {
	if len(chain) == 0 {
		return "", "", errors.New("no certificate")
	}
	roots, inter := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(root)
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	leaf := chain[0]
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return "", "", err
	}
	if !HostNameRe.MatchString(leaf.Subject.CommonName) {
		return "", "", fmt.Errorf("the certificate names %q", leaf.Subject.CommonName)
	}
	return leaf.Subject.CommonName, Fingerprint(leaf), nil
}

// Fingerprint identifies exactly one certificate: the SHA-256 of its DER, hex. Not its serial, which is
// no secret: any key under the root could issue another certificate with the same serial and name.
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return hex.EncodeToString(sum[:])
}

// PEM encodings: certificates as CERTIFICATE, keys as PKCS #8 PRIVATE KEY.

func CertPEM(c *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}

func KeyPEM(k ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func ParseCert(b []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("not a PEM certificate")
	}
	return x509.ParseCertificate(blk.Bytes)
}

func ParseKey(b []byte) (ed25519.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		return nil, errors.New("not a PEM private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("the key is not Ed25519")
	}
	return ek, nil
}

// Load is a CA from its PEM certificate and key, refused unless the key is the certificate's and the
// certificate is a CA's.
func Load(certPEM, keyPEM []byte) (*CA, error) {
	c, err := ParseCert(certPEM)
	if err != nil {
		return nil, err
	}
	k, err := ParseKey(keyPEM)
	if err != nil {
		return nil, err
	}
	if pub, ok := c.PublicKey.(ed25519.PublicKey); !ok || !pub.Equal(k.Public()) {
		return nil, errors.New("the key is not the certificate's")
	}
	if !c.IsCA {
		return nil, errors.New("the certificate is not a CA's")
	}
	return &CA{Cert: c, Key: k}, nil
}
