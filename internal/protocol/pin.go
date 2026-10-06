package protocol

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"time"
)

var errPinMismatch = errors.New("node key does not match the invite")

// Pin is the SHA-256 of a certificate's SubjectPublicKeyInfo: the node's identity.
func Pin(cert *x509.Certificate) [32]byte {
	return sha256.Sum256(cert.RawSubjectPublicKeyInfo)
}

// ClientTLS trusts exactly the certificate whose key matches pin. CA chains and
// names play no part: trust comes from the invite, so a node needs neither.
func ClientTLS(pin [32]byte) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"h2", "http/1.1"},
		// Chain verification is replaced, not skipped: VerifyConnection still runs.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errPinMismatch
			}
			got := Pin(cs.PeerCertificates[0])
			if subtle.ConstantTimeCompare(got[:], pin[:]) != 1 {
				return errPinMismatch
			}
			return nil
		},
	}
}

// ServerTLS serves cert with TLS 1.3 only.
func ServerTLS(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"h2", "http/1.1"},
		Certificates: []tls.Certificate{cert},
	}
}

// NewIdentity generates a node key and a self-signed certificate for it. The
// certificate is long-lived because trust rests on the pinned key, not on dates.
func NewIdentity(now time.Time) (tls.Certificate, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "connverifier node"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(100, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}, nil
}
