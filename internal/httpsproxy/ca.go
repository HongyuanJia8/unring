package httpsproxy

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	caDirectoryName = "ca"
	caCertName      = "ca.pem"
	caKeyName       = "ca-key.pem"
)

// Authority is unring's per-user certificate authority.
type Authority struct {
	Certificate     *x509.Certificate
	PrivateKey      crypto.Signer
	CertificatePath string
	PrivateKeyPath  string
}

// EnsureAuthority loads the existing per-user CA or creates it on first use.
// It never modifies a system trust store.
func EnsureAuthority(stateDir string) (*Authority, error) {
	caDir := filepath.Join(stateDir, caDirectoryName)
	if err := os.MkdirAll(caDir, 0o700); err != nil {
		return nil, fmt.Errorf("create unring CA directory: %w", err)
	}
	if err := os.Chmod(caDir, 0o700); err != nil {
		return nil, fmt.Errorf("restrict unring CA directory: %w", err)
	}
	certPath := filepath.Join(caDir, caCertName)
	keyPath := filepath.Join(caDir, caKeyName)
	if authority, err := loadAuthority(certPath, keyPath); err == nil {
		return authority, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	lockPath := filepath.Join(caDir, ".create.lock")
	lock, err := acquireCALock(lockPath)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = lock.Close()
		_ = os.Remove(lockPath)
	}()

	if authority, err := loadAuthority(certPath, keyPath); err == nil {
		return authority, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return createAuthority(certPath, keyPath)
}

func acquireCALock(path string) (*os.File, error) {
	for attempt := 0; attempt < 100; attempt++ {
		lock, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("lock unring CA creation: %w", err)
		}
		info, statErr := os.Stat(path)
		if statErr == nil && time.Since(info.ModTime()) > time.Minute {
			_ = os.Remove(path)
			continue
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, errors.New("timed out waiting for another unring process to create the CA")
}

func createAuthority(certPath, keyPath string) (*Authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate unring CA private key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "unring local CA",
			Organization: []string{"unring"},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		return nil, fmt.Errorf("create unring CA certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode unring CA private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})

	// Publish the key first. A crash between these writes leaves an explicit
	// incomplete CA that is refused rather than silently regenerated.
	if err := writePrivateFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := writePrivateFile(certPath, certPEM, 0o600); err != nil {
		return nil, err
	}
	return loadAuthority(certPath, keyPath)
}

func writePrivateFile(path string, data []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".ca-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary CA file: %w", err)
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return fmt.Errorf("restrict temporary CA file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary CA file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync temporary CA file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary CA file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("publish CA file: %w", err)
	}
	return nil
}

func loadAuthority(certPath, keyPath string) (*Authority, error) {
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr != nil || keyErr != nil {
		if errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("load unring CA: certificate error: %v; private-key error: %v",
			certErr, keyErr)
	}
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		return nil, fmt.Errorf("inspect unring CA private key: %w", err)
	}
	if keyInfo.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("unring CA private key %s has unsafe permissions %04o",
			keyPath, keyInfo.Mode().Perm())
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, errors.New("decode unring CA certificate: no certificate PEM block")
	}
	certificate, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse unring CA certificate: %w", err)
	}
	if !certificate.IsCA {
		return nil, errors.New("load unring CA: stored certificate is not a CA")
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, errors.New("decode unring CA private key: no PEM block")
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse unring CA private key: %w", err)
	}
	signer, ok := parsedKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("load unring CA: private key cannot sign certificates")
	}
	storedPublic, err := x509.MarshalPKIXPublicKey(certificate.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal unring CA certificate key: %w", err)
	}
	keyPublic, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return nil, fmt.Errorf("marshal unring CA private-key public part: %w", err)
	}
	if !bytes.Equal(storedPublic, keyPublic) {
		return nil, errors.New("load unring CA: certificate and private key do not match")
	}
	return &Authority{
		Certificate: certificate, PrivateKey: signer,
		CertificatePath: certPath, PrivateKeyPath: keyPath,
	}, nil
}

func (authority *Authority) certificateForHost(hostport string) (*tlsCertificate, error) {
	host := hostport
	if splitHost, _, err := net.SplitHostPort(hostport); err == nil {
		host = splitHost
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	notAfter := now.Add(24 * time.Hour)
	if authority.Certificate.NotAfter.Before(notAfter) {
		notAfter = authority.Certificate.NotAfter
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf certificate key for %s: %w", host, err)
	}
	der, err := x509.CreateCertificate(
		rand.Reader, template, authority.Certificate, key.Public(), authority.PrivateKey,
	)
	if err != nil {
		return nil, fmt.Errorf("create leaf certificate for %s: %w", host, err)
	}
	return &tlsCertificate{Certificate: [][]byte{der, authority.Certificate.Raw}, PrivateKey: key}, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial number: %w", err)
	}
	return serial, nil
}
