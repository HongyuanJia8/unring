package httpsproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyj28/unring/internal/adapter"
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
	if summary.Requests[0].Disposition != RequestDispositionSafeRead ||
		summary.HasReviewableActivity() || summary.HasForwardedEffects() {
		t.Fatalf("safe GET manufactured reviewable activity: %#v", summary)
	}
}

func TestSummaryOnlyTreatsForwardedMutationsAsEffects(t *testing.T) {
	t.Parallel()
	safe := Summary{
		Sealed: true,
		Requests: []RequestRecord{{
			Method: http.MethodGet, Disposition: RequestDispositionSafeRead,
		}},
	}
	if safe.HasReviewableActivity() || safe.HasForwardedEffects() {
		t.Fatalf("safe read requires review: %#v", safe)
	}
	controlPlane := Summary{
		Sealed: true,
		Requests: []RequestRecord{{
			Method: http.MethodPost, Disposition: RequestDispositionControlPlane,
		}},
	}
	if controlPlane.HasReviewableActivity() || controlPlane.HasForwardedEffects() {
		t.Fatalf("agent control plane requires review: %#v", controlPlane)
	}
	mutation := Summary{
		Sealed: true,
		Requests: []RequestRecord{{
			Method: http.MethodPost, Disposition: RequestDispositionApproved,
		}},
	}
	if !mutation.HasReviewableActivity() || !mutation.HasForwardedEffects() {
		t.Fatalf("forwarded mutation does not require review: %#v", mutation)
	}
}

func TestAdapterRuleNamesCannotSpoofForwardedDisposition(t *testing.T) {
	adapters, err := adapter.Load(adapter.Source{
		Name: "spoofed-rule-names.yaml",
		Data: []byte(`
version: 1
name: spoofed-rule-names
rules:
  - name: safe-http-method
    match:
      hosts: [spoof.unring.test]
      methods: [POST]
      path: /refund
    tier: needs-approval
  - name: agent-control-plane
    match:
      hosts: [spoof.unring.test]
      methods: [POST]
      path: /payout
    tier: needs-approval
`),
	})
	if err != nil {
		t.Fatalf("load spoofing adapter: %v", err)
	}
	authority, err := EnsureAuthority(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureAuthority() error: %v", err)
	}
	proxy, err := Start(authority, Options{
		Adapters: adapters,
		Approve: func(context.Context, ApprovalRequest) (bool, error) {
			return true, nil
		},
		Transport: proxyRoundTripperFunc(func(*http.Request) (*http.Response, error) {
			const body = "accepted"
			return &http.Response{
				StatusCode: http.StatusCreated, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)),
			}, nil
		}),
	})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })

	proxyURL, _ := url.Parse("http://" + proxy.Address())
	roots := x509.NewCertPool()
	roots.AddCert(authority.Certificate)
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			RootCAs: roots, MinVersion: tls.VersionTLS12,
		},
	}}
	for _, path := range []string{"/refund", "/payout"} {
		response, err := client.Post(
			"https://spoof.unring.test"+path,
			"application/json",
			strings.NewReader(`{"amount":100}`),
		)
		if err != nil {
			t.Fatalf("POST %s through proxy: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("POST %s status = %d", path, response.StatusCode)
		}
	}
	sealProxy(t, proxy)

	summary := proxy.Summary()
	if len(summary.Requests) != 2 || !summary.HasForwardedEffects() ||
		!summary.HasReviewableActivity() {
		t.Fatalf("approved spoof-name mutations lost reviewability: %#v", summary)
	}
	for _, request := range summary.Requests {
		if request.Disposition != RequestDispositionApproved {
			t.Fatalf("adapter rule %q selected disposition %q, want %q",
				request.Rule, request.Disposition, RequestDispositionApproved)
		}
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

func TestForwardingErrorNamesTypedCauseWithoutInspectingURLText(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantPrefix string
	}{
		{
			name: "timeout", err: timeoutTestError{},
			wantPrefix: "timeout during upstream request:",
		},
		{
			name: "typed TLS failure",
			err: tls.RecordHeaderError{
				Msg: "first record does not look like a TLS handshake",
			},
			wantPrefix: "TLS failure during upstream request:",
		},
		{
			name: "DNS failure whose URL contains mtls",
			err: &url.Error{
				Op:  "Get",
				URL: "https://api.vendor.example/v1/mtls/status",
				Err: &net.DNSError{Name: "api.vendor.example", Err: "no such host"},
			},
			wantPrefix: "DNS failure during upstream request:",
		},
		{
			name:       "body relay",
			err:        errors.New("short downstream write"),
			wantPrefix: "upstream request failed:",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := forwardingError("upstream request", test.err)
			if !strings.HasPrefix(got, test.wantPrefix) {
				t.Fatalf("forwardingError() = %q, want prefix %q", got, test.wantPrefix)
			}
		})
	}
}

type timeoutTestError struct{}

func (timeoutTestError) Error() string   { return "deadline elapsed" }
func (timeoutTestError) Timeout() bool   { return true }
func (timeoutTestError) Temporary() bool { return true }

func TestSafeReadStreamsUnknownLengthJSONBeforeEOF(t *testing.T) {
	assertSafeReadStreamsBeforeEOF(t, "application/json", -1)
}

func TestSafeReadStreamsEventSourceBeforeEOF(t *testing.T) {
	assertSafeReadStreamsBeforeEOF(t, "text/event-stream", -1)
}

func TestSafeReadWithContentLengthIsNotBuffered(t *testing.T) {
	const body = "{\"event\":1}\n{\"event\":2}\n"
	assertSafeReadStreamsBeforeEOF(t, "application/json", int64(len(body)))
}

func assertSafeReadStreamsBeforeEOF(t *testing.T, contentType string, contentLength int64) {
	t.Helper()
	upstreamReader, upstreamWriter := io.Pipe()
	releaseUpstream := make(chan struct{})
	firstChunkWritten := make(chan struct{})
	go func() {
		defer upstreamWriter.Close()
		_, _ = io.WriteString(upstreamWriter, "{\"event\":1}\n")
		close(firstChunkWritten)
		<-releaseUpstream
		_, _ = io.WriteString(upstreamWriter, "{\"event\":2}\n")
	}()
	defer close(releaseUpstream)

	authority, err := EnsureAuthority(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureAuthority() error: %v", err)
	}
	proxy, err := Start(authority, Options{Transport: proxyRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{contentType},
			},
			Body: upstreamReader, ContentLength: contentLength,
		}, nil
	})})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })

	proxyURL, _ := url.Parse("http://" + proxy.Address())
	roots := x509.NewCertPool()
	roots.AddCert(authority.Certificate)
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			RootCAs: roots, MinVersion: tls.VersionTLS12,
		},
	}}
	type responseResult struct {
		response *http.Response
		err      error
	}
	result := make(chan responseResult, 1)
	go func() {
		response, requestErr := client.Get("https://stream.unring.test/v1/watch")
		result <- responseResult{response: response, err: requestErr}
	}()

	select {
	case <-firstChunkWritten:
	case <-time.After(time.Second):
		t.Fatal("proxy did not begin reading the unknown-length upstream response")
	}
	var response *http.Response
	select {
	case received := <-result:
		if received.err != nil {
			t.Fatalf("GET streaming safe read: %v", received.err)
		}
		response = received.response
	case <-time.After(250 * time.Millisecond):
		t.Fatal("safe read did not deliver response headers before the upstream body completed")
	}
	defer response.Body.Close()
	if contentLength < 0 {
		if len(response.TransferEncoding) != 1 || response.TransferEncoding[0] != "chunked" {
			t.Fatalf("unknown-length response framing = %#v, want chunked", response.TransferEncoding)
		}
	} else if response.ContentLength != contentLength || len(response.TransferEncoding) != 0 {
		t.Fatalf("known-length response framing: length=%d transfer=%#v",
			response.ContentLength, response.TransferEncoding)
	}

	firstLine := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(response.Body).ReadString('\n')
		firstLine <- line
	}()
	select {
	case line := <-firstLine:
		if line != "{\"event\":1}\n" {
			t.Fatalf("first streamed line = %q", line)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("safe read did not relay the first body chunk before upstream EOF")
	}
}

type proxyRoundTripperFunc func(*http.Request) (*http.Response, error)

func (function proxyRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestConfiguredRootCAsRejectsBundleWithoutCertificates(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "not-a-ca.pem")
	if err := os.WriteFile(filename, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatalf("write invalid CA bundle: %v", err)
	}
	if _, err := configuredRootCAs(filename); err == nil ||
		!strings.Contains(err.Error(), "contains no certificates") {
		t.Fatalf("configuredRootCAs() error = %v", err)
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

func TestProxyTunnelsProtocolUpgradeAndReportsPayloadUnintercepted(t *testing.T) {
	const upgradeProtocol = "unring-echo"
	payload := append([]byte{0x00, 0x81, 0xfe, 0x7f}, bytes.Repeat([]byte("upgrade-frame-"), 2048)...)
	originResult := make(chan error, 1)
	origin := httptest.NewTLSServer(http.HandlerFunc(func(
		response http.ResponseWriter,
		request *http.Request,
	) {
		if !headerHasToken(request.Header, "Connection", "upgrade") ||
			!strings.EqualFold(request.Header.Get("Upgrade"), upgradeProtocol) {
			originResult <- fmt.Errorf(
				"origin received mangled upgrade headers: Connection=%q Upgrade=%q",
				request.Header.Values("Connection"), request.Header.Get("Upgrade"),
			)
			http.Error(response, "upgrade headers missing", http.StatusBadRequest)
			return
		}
		hijacker, ok := response.(http.Hijacker)
		if !ok {
			originResult <- errors.New("origin response cannot be hijacked")
			return
		}
		connection, buffered, err := hijacker.Hijack()
		if err != nil {
			originResult <- fmt.Errorf("hijack origin: %w", err)
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := buffered.WriteString(
			"HTTP/1.1 101 Switching Protocols\r\n" +
				"Connection: Upgrade\r\n" +
				"Upgrade: " + upgradeProtocol + "\r\n\r\n",
		); err != nil {
			originResult <- fmt.Errorf("write origin upgrade response: %w", err)
			return
		}
		if err := buffered.Flush(); err != nil {
			originResult <- fmt.Errorf("flush origin upgrade response: %w", err)
			return
		}
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(buffered, got); err != nil {
			originResult <- fmt.Errorf("read upgraded payload: %w", err)
			return
		}
		if !bytes.Equal(got, payload) {
			originResult <- fmt.Errorf("upgraded payload changed: got %d bytes", len(got))
			return
		}
		if _, err := buffered.Write(got); err != nil {
			originResult <- fmt.Errorf("echo upgraded payload: %w", err)
			return
		}
		if err := buffered.Flush(); err != nil {
			originResult <- fmt.Errorf("flush upgraded payload: %w", err)
			return
		}
		originResult <- nil
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
	t.Cleanup(func() {
		_ = proxy.Close()
	})
	originURL, _ := url.Parse(origin.URL)
	connection, err := net.DialTimeout("tcp", proxy.Address(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	connectReader := bufio.NewReader(connection)
	if _, err := fmt.Fprintf(connection,
		"CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n",
		originURL.Host, originURL.Host,
	); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	connectResponse, err := http.ReadResponse(connectReader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	_ = connectResponse.Body.Close()
	if connectResponse.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", connectResponse.StatusCode)
	}

	host, _, _ := net.SplitHostPort(originURL.Host)
	roots := x509.NewCertPool()
	roots.AddCert(authority.Certificate)
	tlsClient := tls.Client(&readerConn{Conn: connection, reader: connectReader}, &tls.Config{
		RootCAs: roots, ServerName: host, MinVersion: tls.VersionTLS12,
	})
	if err := tlsClient.Handshake(); err != nil {
		t.Fatalf("TLS handshake with proxy: %v", err)
	}
	requestAndPayload := bytes.NewBuffer(nil)
	fmt.Fprintf(requestAndPayload,
		"GET /socket HTTP/1.1\r\nHost: %s\r\n"+
			"Connection: keep-alive, Upgrade\r\nUpgrade: %s\r\n\r\n",
		originURL.Host, upgradeProtocol,
	)
	requestAndPayload.Write(payload)
	if _, err := tlsClient.Write(requestAndPayload.Bytes()); err != nil {
		t.Fatalf("write upgrade request and pipelined payload: %v", err)
	}
	upgradedReader := bufio.NewReader(tlsClient)
	statusLine, err := upgradedReader.ReadString('\n')
	if err != nil {
		t.Fatalf("read upgrade status: %v", err)
	}
	if !strings.Contains(statusLine, " 101 ") {
		t.Fatalf("upgrade status = %q, want 101", strings.TrimSpace(statusLine))
	}
	responseHeaders := make(http.Header)
	for {
		line, err := upgradedReader.ReadString('\n')
		if err != nil {
			t.Fatalf("read upgrade headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
		name, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			t.Fatalf("malformed upgrade response header: %q", line)
		}
		responseHeaders.Add(name, strings.TrimSpace(value))
	}
	if !headerHasToken(responseHeaders, "Connection", "upgrade") ||
		!strings.EqualFold(responseHeaders.Get("Upgrade"), upgradeProtocol) {
		t.Fatalf("proxy mangled upgrade response headers: %#v", responseHeaders)
	}
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(upgradedReader, echo); err != nil {
		t.Fatalf("read echoed upgraded payload: %v", err)
	}
	if !bytes.Equal(echo, payload) {
		t.Fatalf("upgraded payload differs: got %d bytes, want %d", len(echo), len(payload))
	}
	if err := tlsClient.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("close upgraded client: %v", err)
	}
	if err := <-originResult; err != nil {
		t.Fatal(err)
	}
	sealProxy(t, proxy)

	summary := proxy.Summary()
	if len(summary.Requests) != 0 ||
		len(summary.Unintercepted) != 1 ||
		summary.Unintercepted[0].Host != originURL.Host ||
		!strings.Contains(summary.Unintercepted[0].Detail, "tunneled without payload interception") {
		t.Fatalf("protocol upgrade was not honestly reported: %#v", summary)
	}
}

func TestDefaultTransportHonorsInheritedUpstreamProxy(t *testing.T) {
	if os.Getenv("UNRING_UPSTREAM_PROXY_HELPER") == "1" {
		upstreamRequest := make(chan *http.Request, 1)
		upstream := httptest.NewServer(http.HandlerFunc(func(
			response http.ResponseWriter,
			request *http.Request,
		) {
			upstreamRequest <- request.Clone(context.Background())
			http.Error(response, "deliberate upstream refusal", http.StatusBadGateway)
		}))
		defer upstream.Close()
		t.Setenv("HTTPS_PROXY", upstream.URL)
		t.Setenv("https_proxy", upstream.URL)
		t.Setenv("NO_PROXY", "")
		t.Setenv("no_proxy", "")

		authority, err := EnsureAuthority(t.TempDir())
		if err != nil {
			t.Fatalf("EnsureAuthority() error: %v", err)
		}
		proxy, err := Start(authority, Options{})
		if err != nil {
			t.Fatalf("Start() error: %v", err)
		}
		defer proxy.Close()
		proxyURL, _ := url.Parse("http://" + proxy.Address())
		roots := x509.NewCertPool()
		roots.AddCert(authority.Certificate)
		client := &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
				TLSClientConfig: &tls.Config{
					RootCAs: roots, MinVersion: tls.VersionTLS12,
				},
			},
			Timeout: 3 * time.Second,
		}
		_, _ = client.Get("https://origin.unring.invalid/through-upstream")
		select {
		case request := <-upstreamRequest:
			if request.Method != http.MethodConnect ||
				request.Host != "origin.unring.invalid:443" {
				t.Fatalf("upstream request = %s %s, want CONNECT origin.unring.invalid:443",
					request.Method, request.Host)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("inherited upstream proxy never received unring's origin connection")
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	command := exec.CommandContext(
		ctx, os.Args[0],
		"-test.run=^TestDefaultTransportHonorsInheritedUpstreamProxy$",
	)
	command.Env = append(
		withoutEnvironmentKeys(
			os.Environ(),
			"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy",
			"ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
		),
		"UNRING_UPSTREAM_PROXY_HELPER=1",
	)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("upstream proxy helper did not exit: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("upstream proxy helper failed: %v\n%s", err, output)
	}
}

func TestConnectPassthroughHonorsInheritedUpstreamProxy(t *testing.T) {
	if os.Getenv("UNRING_UPSTREAM_TUNNEL_HELPER") == "1" {
		origin := httptest.NewTLSServer(http.HandlerFunc(func(
			response http.ResponseWriter,
			_ *http.Request,
		) {
			_, _ = io.WriteString(response, "tunneled through inherited proxy")
		}))
		defer origin.Close()
		originURL, _ := url.Parse(origin.URL)
		targetHost := net.JoinHostPort("passthrough.unring.test", originURL.Port())
		upstreamSeen := make(chan string, 1)
		upstreamProxy := httptest.NewServer(http.HandlerFunc(func(
			response http.ResponseWriter,
			request *http.Request,
		) {
			upstreamSeen <- request.Host
			hijacker, ok := response.(http.Hijacker)
			if !ok {
				http.Error(response, "cannot tunnel", http.StatusInternalServerError)
				return
			}
			client, buffered, err := hijacker.Hijack()
			if err != nil {
				return
			}
			defer client.Close()
			originConnection, err := net.DialTimeout("tcp", originURL.Host, 2*time.Second)
			if err != nil {
				_, _ = buffered.WriteString("HTTP/1.1 502 Bad Gateway\r\n\r\n")
				_ = buffered.Flush()
				return
			}
			defer originConnection.Close()
			_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
			if err := buffered.Flush(); err != nil {
				return
			}
			toOriginDone := make(chan struct{})
			go func() {
				_, _ = io.Copy(originConnection, buffered)
				if tcp, ok := originConnection.(*net.TCPConn); ok {
					_ = tcp.CloseWrite()
				}
				close(toOriginDone)
			}()
			_, _ = io.Copy(client, originConnection)
			<-toOriginDone
		}))
		defer upstreamProxy.Close()
		t.Setenv("HTTPS_PROXY", upstreamProxy.URL)
		t.Setenv("https_proxy", upstreamProxy.URL)
		t.Setenv("NO_PROXY", "")
		t.Setenv("no_proxy", "")

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
		defer proxy.Close()
		proxyURL, _ := url.Parse("http://" + proxy.Address())
		client := &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, // Isolated test origin only.
				},
			},
			Timeout: 4 * time.Second,
		}
		response, err := client.Get("https://" + targetHost + "/through-upstream")
		if err != nil {
			t.Fatalf("GET through inherited upstream proxy: %v", err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || string(body) != "tunneled through inherited proxy" {
			t.Fatalf("tunneled response = %q, %v", body, err)
		}
		if got := <-upstreamSeen; got != targetHost {
			t.Fatalf("upstream CONNECT host = %q, want %q", got, targetHost)
		}
		sealProxy(t, proxy)
		summary := proxy.Summary()
		if len(summary.Unintercepted) != 1 ||
			summary.Unintercepted[0].Host != targetHost ||
			!strings.Contains(summary.Unintercepted[0].Detail, "passed through") {
			t.Fatalf("passthrough summary = %#v", summary)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(
		ctx, os.Args[0],
		"-test.run=^TestConnectPassthroughHonorsInheritedUpstreamProxy$",
	)
	command.Env = append(
		withoutEnvironmentKeys(
			os.Environ(),
			"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy",
			"ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy",
		),
		"UNRING_UPSTREAM_TUNNEL_HELPER=1",
	)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("upstream tunnel helper did not exit: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("upstream tunnel helper failed: %v\n%s", err, output)
	}
}

func TestSealWaitsForTrackedConnectionActivityBeforeFreezingSummary(t *testing.T) {
	authority, err := EnsureAuthority(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureAuthority() error: %v", err)
	}
	proxy, err := Start(authority, Options{})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	client, peer := net.Pipe()
	defer peer.Close()
	cleanup, tracked := proxy.track(client)
	if !tracked {
		t.Fatal("new connection was rejected before sealing")
	}
	sealResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		sealResult <- proxy.Seal(ctx)
	}()
	select {
	case err := <-sealResult:
		t.Fatalf("Seal returned before tracked work completed: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	proxy.recordUnintercepted("late.example:443",
		"tracked connection completed while the session was sealing")
	cleanup()
	if err := <-sealResult; err != nil {
		t.Fatalf("Seal() error: %v", err)
	}
	summary := proxy.Summary()
	if !summary.Sealed || len(summary.Unintercepted) != 1 ||
		summary.Unintercepted[0].Host != "late.example:443" {
		t.Fatalf("Seal froze before tracked summary activity: %#v", summary)
	}
}

type readerConn struct {
	net.Conn
	reader io.Reader
}

func (connection *readerConn) Read(buffer []byte) (int, error) {
	return connection.reader.Read(buffer)
}

func withoutEnvironmentKeys(environment []string, keys ...string) []string {
	excluded := make(map[string]bool, len(keys))
	for _, key := range keys {
		excluded[key] = true
	}
	result := make([]string, 0, len(environment))
	for _, item := range environment {
		key, _, _ := strings.Cut(item, "=")
		if !excluded[key] {
			result = append(result, item)
		}
	}
	return result
}

func sealProxy(t *testing.T, proxy *Proxy) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := proxy.Seal(ctx); err != nil {
		t.Fatalf("Seal() error: %v", err)
	}
}
