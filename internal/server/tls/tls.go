// Package tls 提供服务端 TLS 配置加载与自签兜底。
package tls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// LoadOrSelfSign 加载指定证书；若 certFile/keyFile 为空且 autoSelfSign 为 true，
// 则生成一个 ECDSA P-256 自签证书（CN=localhost + 127.0.0.1 IP SAN）。
// 返回 (cert, key, error)。
func LoadOrSelfSign(certFile, keyFile string, autoSelfSign bool) (certPEM, keyPEM []byte, err error) {
	if certFile != "" && keyFile != "" {
		cert, err := loadFile(certFile)
		if err != nil {
			return nil, nil, err
		}
		key, err := loadFile(keyFile)
		if err != nil {
			return nil, nil, err
		}
		return cert, key, nil
	}
	if !autoSelfSign {
		return nil, nil, nil
	}
	return selfSign()
}

func selfSign() ([]byte, []byte, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("tls: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("tls: serial: %w", err)
	}
	host, _ := osHostname()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localhost", Organization: []string{"Lujiang"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:     []string{"localhost", host},
		IsCA:         true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("tls: create cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("tls: marshal key: %w", err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	key := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return cert, key, nil
}

func loadFile(p string) ([]byte, error) {
	b, err := readFile(p)
	if err != nil {
		return nil, fmt.Errorf("tls: read %s: %w", p, err)
	}
	return b, nil
}
