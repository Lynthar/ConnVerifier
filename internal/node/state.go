package node

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

const (
	keyFile     = "key.pem"
	certFile    = "cert.pem"
	invitesFile = "invites.json"
)

// DefaultStateDir is where a node keeps its key and invites unless told otherwise.
func DefaultStateDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(".", "connverifier-node")
	}
	return filepath.Join(dir, "connverifier", "node")
}

// InviteLimits cap what one invite's holder may be granted.
type InviteLimits struct {
	MaxSessions     int `json:"max_sessions"`
	MaxConnections  int `json:"max_connections"`
	MaxDialRate     int `json:"max_dial_rate"`
	MaxDurationS    int `json:"max_duration_s"`
	MaxIdleTimeoutS int `json:"max_idle_timeout_s"`
	MaxStampRate    int `json:"max_stamp_rate"` // 0: no STAMP, as for invites made before it existed
	// Load budgets count body bytes in both directions; MaxLoadBytes 0 means no
	// load, as for invites made before it existed.
	MaxLoadBytes       int64 `json:"max_load_bytes"`
	MaxLoadBytesPerDay int64 `json:"max_load_bytes_per_day"`
	MaxLoadConnections int   `json:"max_load_connections"`
}

// DefaultInviteLimits are the per-invite caps when invite create is not told others.
var DefaultInviteLimits = InviteLimits{
	MaxSessions: 2, MaxConnections: 20000, MaxDialRate: 1000,
	MaxDurationS: 24 * 3600, MaxIdleTimeoutS: 3600, MaxStampRate: 100,
	MaxLoadBytes: 2_000_000_000, MaxLoadBytesPerDay: 20_000_000_000, MaxLoadConnections: 48,
}

func (l InviteLimits) Validate() error {
	switch {
	case l.MaxSessions < 1:
		return errors.New("max sessions must be at least 1")
	case l.MaxConnections < 1 || l.MaxConnections > protocol.MaxConnections:
		return fmt.Errorf("max connections must be 1 to %d", protocol.MaxConnections)
	case l.MaxDialRate < 1 || l.MaxDialRate > protocol.MaxDialRate:
		return fmt.Errorf("max dial rate must be 1 to %d", protocol.MaxDialRate)
	case l.MaxDurationS < 1 || l.MaxDurationS > protocol.MaxDurationS:
		return fmt.Errorf("max duration must be 1s to %ds", protocol.MaxDurationS)
	case l.MaxIdleTimeoutS < 1 || l.MaxIdleTimeoutS > protocol.MaxIdleTimeoutS:
		return fmt.Errorf("max idle timeout must be 1s to %ds", protocol.MaxIdleTimeoutS)
	case l.MaxStampRate < 0 || l.MaxStampRate > protocol.MaxStampRate:
		return fmt.Errorf("max STAMP rate must be 0 to %d", protocol.MaxStampRate)
	case l.MaxLoadBytes < 0 || l.MaxLoadBytes > protocol.MaxLoadBytes:
		return fmt.Errorf("max load bytes must be 0 to %d", int64(protocol.MaxLoadBytes))
	case l.MaxLoadBytes > 0 && l.MaxLoadBytesPerDay < 1:
		return errors.New("max load bytes per day must be positive when load is allowed")
	case l.MaxLoadBytes > 0 && (l.MaxLoadConnections < 1 || l.MaxLoadConnections > protocol.MaxLoadConns):
		return fmt.Errorf("max load connections must be 1 to %d", protocol.MaxLoadConns)
	}
	return nil
}

// InviteRecord is what the node keeps of an invite: never the token, only its hash.
type InviteRecord struct {
	Label       string       `json:"label"`
	TokenSHA256 string       `json:"token_sha256"`
	Created     time.Time    `json:"created"`
	Limits      InviteLimits `json:"limits"`
}

type invitesDoc struct {
	Invites []InviteRecord `json:"invites"`
}

func tokenHash(token []byte) string {
	sum := sha256.Sum256(token)
	return hex.EncodeToString(sum[:])
}

// ensureStateDir creates dir owner-only, or refuses one that others can read:
// it holds the node key and the invite hashes.
func ensureStateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return checkPrivate(dir)
}

// checkPrivate refuses a file or directory with group or other permission bits.
// Windows has no such bits, so the check does not apply there.
func checkPrivate(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is accessible to other users (mode %v); restrict it to the owner", path, fi.Mode().Perm())
	}
	return nil
}

// writePrivate replaces path atomically with an owner-only file, so a reader
// never sees it half written.
func writePrivate(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path))
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LoadIdentity returns the node's key and certificate, creating them on first use.
func LoadIdentity(dir string) (tls.Certificate, error) {
	if err := ensureStateDir(dir); err != nil {
		return tls.Certificate{}, err
	}
	keyPath, certPath := filepath.Join(dir, keyFile), filepath.Join(dir, certFile)
	if _, err := os.Stat(keyPath); errors.Is(err, fs.ErrNotExist) {
		return newIdentity(keyPath, certPath)
	}
	if err := checkPrivate(keyPath); err != nil {
		return tls.Certificate{}, err
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("load node identity: %w", err)
	}
	if cert.Leaf == nil {
		if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return tls.Certificate{}, err
		}
	}
	return cert, nil
}

func newIdentity(keyPath, certPath string) (tls.Certificate, error) {
	cert, err := protocol.NewIdentity(time.Now())
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := writePrivate(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	if err := writePrivate(certPath, certPEM); err != nil {
		return tls.Certificate{}, err
	}
	return cert, nil
}

// loadInvites reads the invite list; a missing file is an empty list.
func loadInvites(dir string) ([]InviteRecord, error) {
	path := filepath.Join(dir, invitesFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := checkPrivate(path); err != nil {
		return nil, err
	}
	var doc invitesDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return doc.Invites, nil
}

func saveInvites(dir string, invites []InviteRecord) error {
	data, err := json.MarshalIndent(invitesDoc{Invites: invites}, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(dir, invitesFile), append(data, '\n'))
}

// CreateInvite adds an invite under label and returns the invite string's
// contents. The token exists only in the returned value; the node keeps its hash.
func CreateInvite(dir, label string, addrs []string, udpPort int, limits InviteLimits) (protocol.Invite, error) {
	if err := limits.Validate(); err != nil {
		return protocol.Invite{}, err
	}
	cert, err := LoadIdentity(dir)
	if err != nil {
		return protocol.Invite{}, err
	}
	inv := protocol.Invite{Label: label, Addrs: addrs, Pin: protocol.Pin(cert.Leaf), UDPPort: udpPort}
	if _, err := rand.Read(inv.Token[:]); err != nil {
		return protocol.Invite{}, err
	}
	if err := inv.Validate(); err != nil {
		return protocol.Invite{}, err
	}
	invites, err := loadInvites(dir)
	if err != nil {
		return protocol.Invite{}, err
	}
	for _, r := range invites {
		if r.Label == label {
			return protocol.Invite{}, fmt.Errorf("an invite labelled %q already exists", label)
		}
	}
	invites = append(invites, InviteRecord{
		Label: label, TokenSHA256: tokenHash(inv.Token[:]), Created: time.Now().UTC(), Limits: limits,
	})
	if err := saveInvites(dir, invites); err != nil {
		return protocol.Invite{}, err
	}
	return inv, nil
}

// ListInvites returns the node's invites.
func ListInvites(dir string) ([]InviteRecord, error) { return loadInvites(dir) }

// RevokeInvite deletes the invite labelled label. Sessions it already holds run
// to their end; no new session can be created with it.
func RevokeInvite(dir, label string) error {
	invites, err := loadInvites(dir)
	if err != nil {
		return err
	}
	for i, r := range invites {
		if r.Label == label {
			return saveInvites(dir, append(invites[:i], invites[i+1:]...))
		}
	}
	return fmt.Errorf("no invite labelled %q", label)
}
