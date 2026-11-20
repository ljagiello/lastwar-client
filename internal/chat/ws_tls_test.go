package chat

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"
)

// testTLS is a self-signed certificate for host "chat.test" and a client config trusting it.
func testTLS(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "chat.test"},
		DNSNames:     []string{"chat.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12},
		&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

// Production lines are most likely wss: the handshake runs inside TLS, with the line's host as the
// server name.
func TestDialWSOverTLSUsesTheLineHostAsServerName(t *testing.T) {
	srvConf, cliConf := testTLS(t)
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "chat.test:443" {
			t.Errorf("dialled %q", addr)
		}
		c, s := net.Pipe()
		go func() {
			defer func() { _ = s.Close() }()
			ts := tls.Server(s, srvConf)
			fs := &fakeServer{c: ts, br: bufio.NewReader(ts), headers: map[string]string{}}
			if err := fs.handshake(); err != nil {
				return // the bad-certificate case ends here
			}
			if fs.headers["UID"] != "1" {
				t.Errorf("UID header %q inside TLS", fs.headers["UID"])
			}
			fs.sendJSON(`{"cmd":"login.success"}`)
			fs.drain()
		}()
		return c, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	w, err := dialWS(ctx, dial, cliConf, "wss://chat.test:443", []header{{"UID", "1"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = w.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if msg, err := w.readMessage(); err != nil || string(msg) != `{"cmd":"login.success"}` {
		t.Errorf("got %q, %v", msg, err)
	}
	_ = w.close()

	bad := cliConf.Clone()
	bad.ServerName = "other.test"
	if _, err := dialWS(ctx, dial, bad, "wss://chat.test:443", nil); err == nil {
		t.Error("TLS accepted a certificate for another host")
	}
}
