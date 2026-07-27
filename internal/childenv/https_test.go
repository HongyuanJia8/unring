package childenv

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestHTTPSInjectsProxyAndCAIntoCopy(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	caPath := filepath.Join(directory, "ca.pem")
	nodeCAPath := filepath.Join(directory, "node-existing.pem")
	sslCAPath := filepath.Join(directory, "ssl-existing.pem")
	curlCAPath := filepath.Join(directory, "curl-existing.pem")
	for path, data := range map[string][]byte{
		caPath:     []byte("UNRING CA\n"),
		nodeCAPath: []byte("NODE CORPORATE CA\n"),
		sslCAPath:  []byte("SSL CORPORATE CA\n"),
		curlCAPath: []byte("CURL CORPORATE CA\n"),
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write test CA bundle: %v", err)
		}
	}
	base := []string{
		"KEEP=original", "HTTPS_PROXY=http://old", "HTTP_PROXY=http://old-http",
		"ALL_PROXY=socks5://old-all", "FTP_PROXY=http://old-ftp", "NO_PROXY=*",
		"NODE_EXTRA_CA_CERTS=" + nodeCAPath,
		"SSL_CERT_FILE=" + sslCAPath,
		"CURL_CA_BUNDLE=" + curlCAPath,
	}
	original := append([]string(nil), base...)
	got, err := HTTPS(base, "127.0.0.1:4567", caPath)
	if err != nil {
		t.Fatalf("HTTPS() error: %v", err)
	}
	if !reflect.DeepEqual(base, original) {
		t.Fatalf("HTTPS mutated input: got %v, want %v", base, original)
	}
	environment := environmentMap(got)
	for _, key := range []string{
		"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy",
		"ALL_PROXY", "all_proxy", "FTP_PROXY", "ftp_proxy",
	} {
		if environment[key] != "http://127.0.0.1:4567" {
			t.Fatalf("%s not pointed at loopback proxy: %v", key, environment)
		}
	}
	mergedPath := environment["NODE_EXTRA_CA_CERTS"]
	for _, key := range []string{"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "CURL_CA_BUNDLE"} {
		if environment[key] != mergedPath {
			t.Fatalf("%s does not use merged trust bundle: %v", key, environment)
		}
	}
	merged, err := os.ReadFile(mergedPath)
	if err != nil {
		t.Fatalf("read merged CA bundle: %v", err)
	}
	for _, want := range [][]byte{
		[]byte("NODE CORPORATE CA"), []byte("SSL CORPORATE CA"),
		[]byte("CURL CORPORATE CA"), []byte("UNRING CA"),
	} {
		if !bytes.Contains(merged, want) {
			t.Fatalf("merged CA bundle dropped %q:\n%s", want, merged)
		}
	}
	info, err := os.Stat(mergedPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("merged CA bundle permissions = %v, %v; want 0600", info, err)
	}
	if environment["NO_PROXY"] != "" || environment["no_proxy"] != "" {
		t.Fatalf("inherited proxy bypass remained active: %v", environment)
	}
}

func TestHTTPSRefusesNonLoopbackProxy(t *testing.T) {
	t.Parallel()
	if _, err := HTTPS(nil, "192.0.2.1:8080", "/tmp/ca.pem"); err == nil {
		t.Fatal("HTTPS accepted a non-loopback proxy")
	}
}
