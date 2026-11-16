package queue

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

// testCAPEM builds a self-signed CA certificate as PEM for the DialOpts
// tests (no static cert in the tree).
func testCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "jarvis-test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	block := pem.Block{Type: "CERTIFICATE", Bytes: der}
	return pem.EncodeToMemory(&block)
}

func TestDialOptsTLSConfigUntuned(t *testing.T) {
	cfg, tuned, err := (DialOpts{URL: "amqp://localhost:5672/"}).tlsConfig()
	if err != nil {
		t.Fatalf("tlsConfig: %v", err)
	}
	if tuned || cfg != nil {
		t.Fatalf("plain amqp:// must stay untuned (amqp.Dial path), got tuned=%v cfg=%v", tuned, cfg)
	}
}

func TestDialOptsTLSConfigCA(t *testing.T) {
	ca := testCAPEM(t)
	cfg, tuned, err := (DialOpts{URL: "amqps://remote:5671/", CACert: ca}).tlsConfig()
	if err != nil {
		t.Fatalf("tlsConfig: %v", err)
	}
	if !tuned || cfg == nil {
		t.Fatal("a CA bundle must tune the dial")
	}
	if cfg.RootCAs == nil {
		t.Fatal("CA bundle did not land in RootCAs")
	}
	// The pool must actually contain our CA: its subject DN has to appear
	// in the pool's subjects.
	block, _ := pem.Decode(ca)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse test CA: %v", err)
	}
	found := false
	for _, sb := range cfg.RootCAs.Subjects() {
		if string(sb) == string(cert.RawSubject) {
			found = true
		}
	}
	if !found {
		t.Fatalf("test CA not in the pool (subjects=%d)", len(cfg.RootCAs.Subjects()))
	}
}

func TestDialOptsTLSConfigInsecure(t *testing.T) {
	cfg, tuned, err := (DialOpts{URL: "amqps://remote:5671/", InsecureSkipVerify: true}).tlsConfig()
	if err != nil {
		t.Fatalf("tlsConfig: %v", err)
	}
	if !tuned || cfg == nil || !cfg.InsecureSkipVerify {
		t.Fatalf("insecure flag must produce InsecureSkipVerify, got tuned=%v cfg=%v", tuned, cfg)
	}
}

func TestDialOptsTLSConfigBadCA(t *testing.T) {
	_, _, err := (DialOpts{CACert: []byte("not a pem bundle")}).tlsConfig()
	if err == nil {
		t.Fatal("a non-PEM CA bundle must be rejected (a typo'd ca_file must not silently fall back to system roots)")
	}
}
