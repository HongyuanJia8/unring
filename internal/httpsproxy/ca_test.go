package httpsproxy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureAuthorityCreatesPrivateReusableCA(t *testing.T) {
	stateDir := t.TempDir()
	first, err := EnsureAuthority(stateDir)
	if err != nil {
		t.Fatalf("EnsureAuthority(first) error: %v", err)
	}
	firstCertificate, err := os.ReadFile(first.CertificatePath)
	if err != nil {
		t.Fatalf("read first certificate: %v", err)
	}
	keyInfo, err := os.Stat(first.PrivateKeyPath)
	if err != nil {
		t.Fatalf("stat private key: %v", err)
	}
	if got := keyInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("private key mode = %04o, want 0600", got)
	}
	directoryInfo, err := os.Stat(filepath.Dir(first.PrivateKeyPath))
	if err != nil {
		t.Fatalf("stat CA directory: %v", err)
	}
	if got := directoryInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("CA directory mode = %04o, want 0700", got)
	}

	second, err := EnsureAuthority(stateDir)
	if err != nil {
		t.Fatalf("EnsureAuthority(second) error: %v", err)
	}
	secondCertificate, err := os.ReadFile(second.CertificatePath)
	if err != nil {
		t.Fatalf("read second certificate: %v", err)
	}
	if string(firstCertificate) != string(secondCertificate) ||
		first.Certificate.SerialNumber.Cmp(second.Certificate.SerialNumber) != 0 {
		t.Fatal("second EnsureAuthority call regenerated the CA")
	}
}

func TestEnsureAuthorityRefusesUnsafePrivateKeyPermissions(t *testing.T) {
	stateDir := t.TempDir()
	authority, err := EnsureAuthority(stateDir)
	if err != nil {
		t.Fatalf("EnsureAuthority() error: %v", err)
	}
	if err := os.Chmod(authority.PrivateKeyPath, 0o644); err != nil {
		t.Fatalf("make key permissions unsafe: %v", err)
	}
	if _, err := EnsureAuthority(stateDir); err == nil {
		t.Fatal("EnsureAuthority accepted a world-readable private key")
	}
}
