package childenv

import (
	"reflect"
	"testing"
)

func TestHTTPSInjectsProxyAndCAIntoCopy(t *testing.T) {
	t.Parallel()
	base := []string{
		"KEEP=original", "HTTPS_PROXY=http://old", "NO_PROXY=*",
		"NODE_EXTRA_CA_CERTS=/old/ca.pem",
	}
	original := append([]string(nil), base...)
	got, err := HTTPS(base, "127.0.0.1:4567", "/private/state/unring/ca/ca.pem")
	if err != nil {
		t.Fatalf("HTTPS() error: %v", err)
	}
	if !reflect.DeepEqual(base, original) {
		t.Fatalf("HTTPS mutated input: got %v, want %v", base, original)
	}
	environment := environmentMap(got)
	if environment["HTTPS_PROXY"] != "http://127.0.0.1:4567" ||
		environment["https_proxy"] != environment["HTTPS_PROXY"] {
		t.Fatalf("proxy environment not injected: %v", environment)
	}
	for _, key := range []string{"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "CURL_CA_BUNDLE"} {
		if environment[key] != "/private/state/unring/ca/ca.pem" {
			t.Fatalf("%s not injected: %v", key, environment)
		}
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
