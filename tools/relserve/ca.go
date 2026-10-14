package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"time"
)

// certLifetime is how long the CA and the server certificate are valid. A test run takes minutes; two days leave
// room for a run that crosses midnight and for a clock that is a little off inside a container or a VM.
const certLifetime = 48 * time.Hour

// authority is a throwaway certificate authority and the one server certificate it signed.
type authority struct {
	certPEM []byte          // the CA certificate, for clients to trust
	server  tls.Certificate // the server's certificate and key, with the CA certificate as its chain
}

// newAuthority makes a CA and a server certificate for hosts (DNS names and IP addresses), valid from an hour
// before now. The CA's key is used once, here, and then dropped.
func newAuthority(hosts []string, now time.Time) (*authority, error) {
	if len(hosts) == 0 {
		return nil, errors.New("no host names for the server certificate")
	}
	notBefore, notAfter := now.Add(-time.Hour), now.Add(certLifetime)

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	caSerial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "isshoni relserve test CA", Organization: []string{"isshoni tests"}},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	leafSerial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber:          leafSerial,
		Subject:               pkix.Name{CommonName: "isshoni relserve"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			leafTmpl.IPAddresses = append(leafTmpl.IPAddresses, ip)
		} else {
			leafTmpl.DNSNames = append(leafTmpl.DNSNames, h)
		}
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}

	return &authority{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		server:  tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey},
	}, nil
}

// serverConfig is the TLS configuration of the server: TLS 1.2 or newer, which is what the installer asks its
// downloader for (06 §4.3).
func (a *authority) serverConfig() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{a.server},
		MinVersion:   tls.VersionTLS12,
	}
}

// randomSerial returns a random positive 127-bit serial number.
func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}
