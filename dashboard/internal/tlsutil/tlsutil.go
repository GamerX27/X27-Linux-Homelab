// Package tlsutil makes the self-signed certificate the dashboard serves, and pins it:
// a main node remembers each node's certificate fingerprint when it pairs, and refuses
// to talk to that node if it ever changes.
package tlsutil

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
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Skip leaves an address out of LocalIPs (the virtual IP, which comes and goes).
var Skip func(ip string) bool

// Ensure loads dir/cert.pem + dir/key.pem, making them first if needed.
func Ensure(dir string) (tls.Certificate, error) {
	c, _, err := EnsureIPs(dir, nil)
	return c, err
}

// EnsureIPs is Ensure for a certificate that must also name these addresses (the virtual
// IP): one that doesn't is made again, with a new fingerprint. changed says it was.
func EnsureIPs(dir string, extra []string) (c tls.Certificate, changed bool, err error) {
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if leaf, err := x509.ParseCertificate(c.Certificate[0]); err == nil && hasIPs(leaf, extra) {
			return c, false, nil
		}
	}
	c, err = generate(dir, extra)
	return c, err == nil, err
}

func hasIPs(leaf *x509.Certificate, ips []string) bool {
	for _, want := range ips {
		found := false
		for _, have := range leaf.IPAddresses {
			found = found || have.String() == want
		}
		if !found {
			return false
		}
	}
	return true
}

func generate(dir string, extra []string) (tls.Certificate, error) {
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	host, _ := os.Hostname()
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host, Organization: []string{"dashboard"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{host, "localhost"},
	}
	for _, ip := range append(LocalIPs(), extra...) {
		if p := net.ParseIP(ip); p != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, p)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return tls.Certificate{}, err
	}
	return tls.LoadX509KeyPair(certPath, keyPath)
}

// Fingerprint is the SHA-256 of the leaf certificate, as colon-separated hex.
func Fingerprint(c tls.Certificate) string {
	if len(c.Certificate) == 0 {
		return ""
	}
	return FingerprintDER(c.Certificate[0])
}

func FingerprintDER(der []byte) string {
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	parts := make([]string, 0, len(h)/2)
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// PinnedConfig accepts only a server whose leaf certificate has this fingerprint.
// An empty fingerprint accepts any certificate and is only used while pairing,
// with the caller reading the fingerprint back through Seen.
func PinnedConfig(fingerprint string, seen *string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, // replaced by the pin check below
		MinVersion:         tls.VersionTLS12,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no certificate")
			}
			got := FingerprintDER(cs.PeerCertificates[0].Raw)
			if seen != nil {
				*seen = got
			}
			if fingerprint != "" && got != fingerprint {
				return fmt.Errorf("certificate changed (expected %s, got %s)", fingerprint, got)
			}
			return nil
		},
	}
}

// LocalIPs lists the host's addresses, leaving out loopback and Docker bridges.
func LocalIPs() []string {
	var out []string
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		if strings.HasPrefix(i.Name, "docker") || strings.HasPrefix(i.Name, "br-") || strings.HasPrefix(i.Name, "veth") {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLinkLocalUnicast() || Skip != nil && Skip(ipn.IP.String()) {
				continue
			}
			out = append(out, ipn.IP.String())
		}
	}
	return out
}
