package httpsproxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestProxyInterceptsRecordsAndForwardsHTTPS(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/record-me" {
			t.Errorf("origin path = %q, want /record-me", request.URL.Path)
		}
		response.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(response, "forwarded")
	}))
	defer origin.Close()

	authority, err := EnsureAuthority(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureAuthority() error: %v", err)
	}
	proxy, err := Start(authority, Options{Transport: origin.Client().Transport})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	proxyURL, _ := url.Parse("http://" + proxy.Address())
	roots := x509.NewCertPool()
	roots.AddCert(authority.Certificate)
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			RootCAs: roots, MinVersion: tls.VersionTLS12,
		},
	}}
	response, err := client.Get(origin.URL + "/record-me")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusCreated || string(body) != "forwarded" {
		t.Fatalf("response = %d %q", response.StatusCode, body)
	}
	sealProxy(t, proxy)

	summary := proxy.Summary()
	if len(summary.Requests) != 1 || summary.Requests[0].Method != http.MethodGet ||
		summary.Requests[0].StatusCode != http.StatusCreated ||
		!strings.Contains(summary.Requests[0].URL, "/record-me") {
		t.Fatalf("unexpected request summary: %#v", summary)
	}
	if len(summary.Unintercepted) != 0 {
		t.Fatalf("successful interception was reported as uncovered: %#v", summary.Unintercepted)
	}
}

func TestPrepareClientResponseAddsHTTP11Framing(t *testing.T) {
	t.Parallel()

	request := &http.Request{Method: http.MethodGet, ProtoMajor: 1, ProtoMinor: 1}
	response := &http.Response{
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/2.0",
		ProtoMajor:    2,
		ContentLength: -1,
		Body:          io.NopCloser(strings.NewReader("body")),
		Uncompressed:  true,
	}
	if closeClient := prepareClientResponse(response, request); closeClient {
		t.Fatal("keep-alive response unexpectedly requires a client close")
	}
	if response.Proto != "HTTP/1.1" ||
		len(response.TransferEncoding) != 1 ||
		response.TransferEncoding[0] != "chunked" ||
		response.Uncompressed {
		t.Fatalf("unknown-length response was not re-framed for HTTP/1.1: %#v", response)
	}

	request.Close = true
	response.TransferEncoding = nil
	if closeClient := prepareClientResponse(response, request); !closeClient || !response.Close {
		t.Fatalf("connection-close response was not marked for closure: %#v", response)
	}
}

func TestProxyReportsClientThatDoesNotTrustCA(t *testing.T) {
	authority, err := EnsureAuthority(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureAuthority() error: %v", err)
	}
	proxy, err := Start(authority, Options{})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	proxyURL, _ := url.Parse("http://" + proxy.Address())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	_, err = client.Get("https://untrusted-client.example/")
	if err == nil {
		t.Fatal("client without unring CA unexpectedly trusted interception")
	}
	sealProxy(t, proxy)

	summary := proxy.Summary()
	if len(summary.Unintercepted) != 1 ||
		summary.Unintercepted[0].Host != "untrusted-client.example:443" ||
		!strings.Contains(summary.Unintercepted[0].Detail, "may not trust") {
		t.Fatalf("untrusted client was not honestly reported: %#v", summary)
	}
}

func TestProxyReportsExplicitConnectPassthrough(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(response, "tunneled")
	}))
	defer origin.Close()
	authority, err := EnsureAuthority(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureAuthority() error: %v", err)
	}
	proxy, err := Start(authority, Options{
		PassthroughHost: func(string) bool { return true },
	})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	proxyURL, _ := url.Parse("http://" + proxy.Address())
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // Test origin only; passthrough preserves its certificate.
		},
	}}
	response, err := client.Get(origin.URL)
	if err != nil {
		t.Fatalf("GET through passthrough: %v", err)
	}
	_ = response.Body.Close()
	sealProxy(t, proxy)

	summary := proxy.Summary()
	if len(summary.Requests) != 0 || len(summary.Unintercepted) != 1 ||
		!strings.Contains(summary.Unintercepted[0].Detail, "passed through") ||
		summary.Unintercepted[0].Host == "" {
		t.Fatalf("CONNECT passthrough was not reported with its host: %#v", summary)
	}
}

func sealProxy(t *testing.T, proxy *Proxy) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := proxy.Seal(ctx); err != nil {
		t.Fatalf("Seal() error: %v", err)
	}
}
