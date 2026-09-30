package peer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Identity is one side's key pair: its certificate, and the pin the other
// side keeps (SPKI).
type Identity struct {
	Cert tls.Certificate
	PEM  string // the certificate alone, what the other side is given
	SPKI string
}

// SPKI is the pin for a certificate: hex SHA-256 of its
// SubjectPublicKeyInfo. The key, not the certificate: nothing else in it
// is checked.
func SPKI(c *x509.Certificate) string {
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// PrepareDir creates dir 0700 if missing, then insists, via Lstat so a
// symlink is seen as one: a directory, ours, mode exactly 0700. The same
// rule as the control socket's directory.
func PrepareDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return fmt.Errorf("peer: %w", err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("peer: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("peer: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("peer: %s is not a directory (%v); remove it", dir, fi.Mode().Type())
	}
	if uid, ok := ownerOf(fi); !ok || uid != os.Getuid() {
		return fmt.Errorf("peer: %s is owned by uid %d, not us", dir, uid)
	}
	if m := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); m != 0o700 {
		return fmt.Errorf("peer: %s has mode %v, want exactly 0700; chmod 700 it", dir, m)
	}
	return nil
}

// keyFile holds the private key and certificate together, so one rename
// makes both at once.
const keyFile = "server.pem"

// LoadOrCreateServer is the server's identity in dir
// ($XDG_STATE_HOME/pneu/peer), made on first need. The directory and the
// file are checked on every load.
func LoadOrCreateServer(dir, host string) (Identity, error) {
	if err := PrepareDir(dir); err != nil {
		return Identity{}, err
	}
	path := filepath.Join(dir, keyFile)
	id, err := loadIdentity(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return id, err
	}
	b, err := newIdentityPEM("pneu " + host)
	if err != nil {
		return Identity{}, err
	}
	// Link, not rename: a racing first run keeps whichever landed first.
	tmp, err := os.CreateTemp(dir, ".server-*")
	if err != nil {
		return Identity{}, fmt.Errorf("peer: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return Identity{}, err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return Identity{}, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return Identity{}, err
	}
	if err := tmp.Close(); err != nil {
		return Identity{}, err
	}
	if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, fs.ErrExist) {
		return Identity{}, fmt.Errorf("peer: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return loadIdentity(path)
}

func loadIdentity(path string) (Identity, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return Identity{}, err
	}
	defer f.Close()
	if err := checkPrivate(f, path); err != nil {
		return Identity{}, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return Identity{}, err
	}
	cert, err := tls.X509KeyPair(b, b)
	if err != nil {
		return Identity{}, fmt.Errorf("peer: %s: %w", path, err)
	}
	if len(cert.Certificate) != 1 {
		return Identity{}, fmt.Errorf("peer: %s: want one certificate", path)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return Identity{}, fmt.Errorf("peer: %s: %w", path, err)
	}
	if !isP256(leaf) {
		return Identity{}, fmt.Errorf("peer: %s: not an ECDSA P-256 key", path)
	}
	cert.Leaf = leaf
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}))
	return Identity{Cert: cert, PEM: certPEM, SPKI: SPKI(leaf)}, nil
}

// newIdentityPEM is a fresh ECDSA P-256 key and a self-signed certificate
// for it, both PEM. Only the key is ever pinned; the certificate's names
// and dates are there because TLS needs a certificate, and nobody checks
// them.
func newIdentityPEM(cn string) ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: strings.ToValidUTF8(cn, "?")},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(30, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})
	return append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...), nil
}

func isP256(c *x509.Certificate) bool {
	k, ok := c.PublicKey.(*ecdsa.PublicKey)
	return ok && k.Curve == elliptic.P256()
}

// ParseCertPEM is a peer's certificate as pairing hands it over: the
// input, trimmed of whitespace, is exactly one PEM CERTIFICATE block with no
// headers, parsing to one X.509 certificate with an ECDSA P-256 key.
// pem.Decode skips anything it can't read up to a later block, so the
// block must start the input and be its only BEGIN line.
func ParseCertPEM(s string) (*x509.Certificate, error) {
	t := strings.TrimSpace(s)
	if strings.Count(t, "-----BEGIN ") != 1 || !strings.HasPrefix(t, "-----BEGIN CERTIFICATE-----\n") && !strings.HasPrefix(t, "-----BEGIN CERTIFICATE-----\r\n") {
		return nil, errors.New("certificate: not exactly one PEM CERTIFICATE block")
	}
	block, rest := pem.Decode([]byte(t))
	switch {
	case block == nil:
		return nil, errors.New("certificate: malformed PEM")
	case block.Type != "CERTIFICATE":
		return nil, fmt.Errorf("certificate: a %q block", block.Type)
	case len(block.Headers) != 0:
		return nil, errors.New("certificate: PEM headers")
	case len(rest) != 0:
		return nil, errors.New("certificate: trailing data")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("certificate: %w", err)
	}
	if !isP256(c) {
		return nil, errors.New("certificate: not an ECDSA P-256 key")
	}
	return c, nil
}
