package httpsproxy_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hyj28/unring/internal/childenv"
	"github.com/hyj28/unring/internal/httpsproxy"
)

func TestCurlReceivesCompleteResponsesIntegration(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skipf("curl unavailable: %v", err)
	}

	chunkedBody := repeatedBody("chunked-response-", 512*1024)
	unframedBody := repeatedBody("http2-unframed-response-", 384*1024)
	keepAliveBodyOne := repeatedBody("keep-alive-one-", 96*1024)
	keepAliveBodyTwo := repeatedBody("keep-alive-two-", 128*1024)
	closeBody := repeatedBody("connection-close-", 192*1024)
	postBody := repeatedBody("post-request-", 256*1024)
	postResponse := repeatedBody("post-response-", 160*1024)

	origin := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/unframed":
			_, _ = response.Write(unframedBody)
		case "/chunked":
			flusher, ok := response.(http.Flusher)
			if !ok {
				t.Error("origin response does not support flushing")
				return
			}
			for offset := 0; offset < len(chunkedBody); offset += 4093 {
				end := min(offset+4093, len(chunkedBody))
				_, _ = response.Write(chunkedBody[offset:end])
				flusher.Flush()
			}
		case "/keep-one":
			response.Header().Set("Content-Length", fmt.Sprint(len(keepAliveBodyOne)))
			_, _ = response.Write(keepAliveBodyOne)
		case "/keep-two":
			response.Header().Set("Content-Length", fmt.Sprint(len(keepAliveBodyTwo)))
			_, _ = response.Write(keepAliveBodyTwo)
		case "/close":
			response.Header().Set("Connection", "close")
			flusher, ok := response.(http.Flusher)
			if !ok {
				t.Error("origin response does not support flushing")
				return
			}
			for offset := 0; offset < len(closeBody); offset += 3571 {
				end := min(offset+3571, len(closeBody))
				_, _ = response.Write(closeBody[offset:end])
				flusher.Flush()
			}
		case "/post":
			got, readErr := io.ReadAll(request.Body)
			if readErr != nil {
				t.Errorf("origin read POST body: %v", readErr)
				return
			}
			if !bytes.Equal(got, postBody) {
				t.Errorf("origin POST body differs: got %d bytes, want %d", len(got), len(postBody))
				return
			}
			response.Header().Set("Content-Length", fmt.Sprint(len(postResponse)))
			_, _ = response.Write(postResponse)
		default:
			http.NotFound(response, request)
		}
	}))
	defer origin.Close()

	proxy, environment, targetAddress := startRuntimeProxy(t, origin)

	got := runCurl(t, curl, environment, nil,
		"--http1.1", "https://"+targetAddress+"/unframed")
	assertBytesEqual(t, "HTTP/2-style unframed response", got, unframedBody)

	got = runCurl(t, curl, environment, nil,
		"--http1.1", "https://"+targetAddress+"/chunked")
	assertBytesEqual(t, "chunked response", got, chunkedBody)

	connectionsBefore := proxy.Summary().Connections
	got = runCurl(t, curl, environment, nil, "--http1.1",
		"https://"+targetAddress+"/keep-one",
		"https://"+targetAddress+"/keep-two",
	)
	assertBytesEqual(t, "keep-alive responses", got,
		append(append([]byte(nil), keepAliveBodyOne...), keepAliveBodyTwo...))
	if added := proxy.Summary().Connections - connectionsBefore; added != 1 {
		t.Fatalf("two keep-alive requests used %d CONNECT connections, want 1", added)
	}

	got = runCurl(t, curl, environment, nil, "--http1.1",
		"--header", "Connection: close",
		"https://"+targetAddress+"/close",
	)
	assertBytesEqual(t, "connection-close response", got, closeBody)

	got = runCurl(t, curl, environment, postBody, "--http1.1",
		"--request", "POST", "--data-binary", "@-",
		"https://"+targetAddress+"/post",
	)
	assertBytesEqual(t, "POST response", got, postResponse)

	sealContext, sealCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer sealCancel()
	if err := proxy.Seal(sealContext); err != nil {
		t.Fatalf("Seal() error: %v", err)
	}
	summary := proxy.Summary()
	if len(summary.Requests) != 6 {
		t.Fatalf("recorded requests = %d, want 6: %#v", len(summary.Requests), summary)
	}
	for _, request := range summary.Requests {
		if request.StatusCode != http.StatusOK || request.Error != "" {
			t.Fatalf("failed curl request was recorded: %#v", request)
		}
	}
}

func TestCurlAndNodeChildrenUseInjectedHTTPSProxyIntegration(t *testing.T) {
	curl, curlErr := exec.LookPath("curl")
	node, nodeErr := exec.LookPath("node")
	if curlErr != nil || nodeErr != nil {
		t.Skipf("curl or node unavailable (curl: %v, node: %v)", curlErr, nodeErr)
	}
	origin := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(response, "through-unring:"+request.URL.Path)
	}))
	defer origin.Close()
	proxy, environment, targetAddress := startRuntimeProxy(t, origin)
	targetHost, targetPort, err := net.SplitHostPort(targetAddress)
	if err != nil {
		t.Fatalf("split target address: %v", err)
	}

	curlOutput := runCurl(t, curl, environment, nil, "https://"+targetAddress+"/curl")
	if string(curlOutput) != "through-unring:/curl" {
		t.Fatalf("curl output = %q", curlOutput)
	}

	proxyHost, proxyPort, _ := net.SplitHostPort(proxy.Address())
	nodeScript := `
const net = require("net");
const tls = require("tls");
const proxyHost = process.argv[1], proxyPort = Number(process.argv[2]);
const targetHost = process.argv[3], targetPort = Number(process.argv[4]);
const socket = net.connect(proxyPort, proxyHost, () => {
  socket.write("CONNECT " + targetHost + ":" + targetPort +
    " HTTP/1.1\r\nHost: " + targetHost + ":" + targetPort + "\r\n\r\n");
});
let buffered = Buffer.alloc(0);
socket.on("data", function connected(chunk) {
  buffered = Buffer.concat([buffered, chunk]);
  const end = buffered.indexOf("\r\n\r\n");
  if (end < 0) return;
  socket.removeListener("data", connected);
  if (!buffered.toString("utf8", 0, end).startsWith("HTTP/1.1 200")) process.exit(3);
  const secure = tls.connect({socket, servername: targetHost}, () => {
    secure.write("GET /node HTTP/1.1\r\nHost: " + targetHost +
      "\r\nConnection: close\r\n\r\n");
  });
  let response = "";
  secure.on("data", chunk => response += chunk.toString());
  secure.on("end", () => {
    process.stdout.write(response.slice(response.indexOf("\r\n\r\n") + 4));
  });
  secure.on("error", error => { console.error(error.message); process.exit(4); });
});
socket.on("error", error => { console.error(error.message); process.exit(2); });
`
	nodeContext, nodeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer nodeCancel()
	nodeCommand := exec.CommandContext(
		nodeContext, node, "-e", nodeScript, proxyHost, proxyPort, targetHost, targetPort,
	)
	nodeCommand.Env = environment
	nodeOutput, err := nodeCommand.CombinedOutput()
	if nodeContext.Err() != nil {
		t.Fatalf("node did not exit within 5 seconds: %v\n%s", nodeContext.Err(), nodeOutput)
	}
	if err != nil {
		t.Fatalf("node through unring: %v\n%s", err, nodeOutput)
	}
	if string(nodeOutput) != "through-unring:/node" {
		t.Fatalf("node output = %q", nodeOutput)
	}

	sealContext, sealCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer sealCancel()
	if err := proxy.Seal(sealContext); err != nil {
		t.Fatalf("Seal() error: %v", err)
	}
	summary := proxy.Summary()
	if len(summary.Requests) != 2 {
		t.Fatalf("recorded requests = %d, want 2: %#v", len(summary.Requests), summary)
	}
	seen := make(map[string]bool)
	for _, request := range summary.Requests {
		if request.StatusCode != http.StatusOK || request.Error != "" {
			t.Fatalf("failed runtime request was recorded: %#v", request)
		}
		seen[request.URL] = true
	}
	for _, path := range []string{"/curl", "/node"} {
		want := fmt.Sprintf("https://%s%s", targetAddress, path)
		if !seen[want] {
			t.Fatalf("audit omitted %s; recorded URLs: %s", want, strings.Join(mapKeys(seen), ", "))
		}
	}
}

func TestCurlSafeReadDoesNotRequireReviewIntegration(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skipf("curl unavailable: %v", err)
	}
	origin := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(response, "read-only")
	}))
	defer origin.Close()
	proxy, environment, targetAddress := startRuntimeProxy(t, origin)

	output := runCurl(t, curl, environment, nil, "https://"+targetAddress+"/read")
	if string(output) != "read-only" {
		t.Fatalf("curl output = %q, want read-only", output)
	}
	sealContext, sealCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer sealCancel()
	if err := proxy.Seal(sealContext); err != nil {
		t.Fatalf("Seal() error: %v", err)
	}
	summary := proxy.Summary()
	if len(summary.Requests) != 1 ||
		summary.Requests[0].Disposition != httpsproxy.RequestDispositionSafeRead ||
		summary.HasReviewableActivity() {
		t.Fatalf("child HTTPS GET manufactured reviewable activity: %#v", summary)
	}
}

func TestCurlAgentControlPlanePostIsForwardedAndVisibleIntegration(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skipf("curl unavailable: %v", err)
	}
	origin := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/messages" {
			t.Errorf("origin request = %s %s", request.Method, request.URL.Path)
		}
		response.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(response, "model-response")
	}))
	defer origin.Close()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatalf("parse origin URL: %v", err)
	}
	targetAddress := net.JoinHostPort("api.anthropic.com", originURL.Port())
	upstream := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", originURL.Host)
		},
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // Isolated httptest origin only.
		},
	}
	authority, err := httpsproxy.EnsureAuthority(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureAuthority() error: %v", err)
	}
	approvalCalls := 0
	proxy, err := httpsproxy.Start(authority, httpsproxy.Options{
		Transport: upstream,
		AgentControlPlane: func(request *http.Request) bool {
			return request.Method == http.MethodPost &&
				request.URL.Hostname() == "api.anthropic.com" &&
				request.URL.Path == "/v1/messages"
		},
		Approve: func(context.Context, httpsproxy.ApprovalRequest) (bool, error) {
			approvalCalls++
			return false, nil
		},
	})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	environment, err := childenv.HTTPS(os.Environ(), proxy.Address(), authority.CertificatePath)
	if err != nil {
		t.Fatalf("build child environment: %v", err)
	}

	output := runCurl(t, curl, environment, []byte(`{"model":"claude"}`),
		"--request", "POST", "--data-binary", "@-",
		"https://"+targetAddress+"/v1/messages")
	if string(output) != "model-response" {
		t.Fatalf("control-plane response = %q, want model-response", output)
	}
	sealContext, sealCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer sealCancel()
	if err := proxy.Seal(sealContext); err != nil {
		t.Fatalf("Seal() error: %v", err)
	}
	summary := proxy.Summary()
	if approvalCalls != 0 || len(summary.Approvals) != 0 ||
		len(summary.Requests) != 1 ||
		summary.Requests[0].Disposition != httpsproxy.RequestDispositionControlPlane ||
		summary.Requests[0].StatusCode != http.StatusAccepted ||
		summary.HasReviewableActivity() {
		t.Fatalf("control-plane POST was gated, hidden, or made reviewable: %#v", summary)
	}
}

func TestPlainHTTPCurlCannotEscapeUnreportedIntegration(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skipf("curl unavailable: %v", err)
	}
	originReached := make(chan struct{}, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		originReached <- struct{}{}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer origin.Close()

	authority, err := httpsproxy.EnsureAuthority(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureAuthority() error: %v", err)
	}
	proxy, err := httpsproxy.Start(authority, httpsproxy.Options{})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	t.Cleanup(func() {
		_ = proxy.Close()
	})
	environment, err := childenv.HTTPS(os.Environ(), proxy.Address(), authority.CertificatePath)
	if err != nil {
		t.Fatalf("build child environment: %v", err)
	}
	body := runCurl(t, curl, environment, nil,
		"--request", "DELETE", origin.URL+"/v1/orders/42")
	if !strings.Contains(string(body), "unring blocked plain HTTP") {
		t.Fatalf("plain HTTP response did not explain the fail-closed result: %q", body)
	}
	select {
	case <-originReached:
		t.Fatal("plain HTTP request escaped unring and reached its origin")
	default:
	}

	sealContext, sealCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer sealCancel()
	if err := proxy.Seal(sealContext); err != nil {
		t.Fatalf("Seal() error: %v", err)
	}
	summary := proxy.Summary()
	originURL, _ := url.Parse(origin.URL)
	if len(summary.Requests) != 0 ||
		len(summary.Unintercepted) != 1 ||
		summary.Unintercepted[0].Host != originURL.Host ||
		!strings.Contains(summary.Unintercepted[0].Detail, "plain HTTP") {
		t.Fatalf("plain HTTP was not visibly reported with its host: %#v", summary)
	}
}

func TestGoBinaryWithoutKeychainTrustIsReportedUninterceptedIntegration(t *testing.T) {
	if os.Getenv("UNRING_GO_UNTRUSTED_HELPER") == "1" {
		transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
		client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
		response, err := client.Get("https://go-client.unring.test/")
		if err != nil {
			return
		}
		_ = response.Body.Close()
		t.Fatalf("Go helper unexpectedly completed with HTTP %d", response.StatusCode)
	}
	if runtime.GOOS != "darwin" {
		t.Skip("Go's SSL_CERT_FILE limitation under test is specific to macOS")
	}

	authority, err := httpsproxy.EnsureAuthority(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureAuthority() error: %v", err)
	}
	proxy, err := httpsproxy.Start(authority, httpsproxy.Options{})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	t.Cleanup(func() {
		_ = proxy.Close()
	})
	environment, err := childenv.HTTPS(os.Environ(), proxy.Address(), authority.CertificatePath)
	if err != nil {
		t.Fatalf("build child environment: %v", err)
	}
	environment = append(environment, "UNRING_GO_UNTRUSTED_HELPER=1")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(
		ctx, os.Args[0],
		"-test.run=^TestGoBinaryWithoutKeychainTrustIsReportedUninterceptedIntegration$",
	)
	command.Env = environment
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("Go helper did not exit within 5 seconds: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("Go helper did not observe the expected trust failure: %v\n%s", err, output)
	}

	sealContext, sealCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer sealCancel()
	if err := proxy.Seal(sealContext); err != nil {
		t.Fatalf("Seal() error: %v", err)
	}
	summary := proxy.Summary()
	if len(summary.Requests) != 0 || len(summary.Unintercepted) != 1 ||
		summary.Unintercepted[0].Host != "go-client.unring.test:443" ||
		!strings.Contains(summary.Unintercepted[0].Detail, "may not trust") {
		t.Fatalf("Go client's trust failure was not reported with its host: %#v", summary)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func startRuntimeProxy(
	t *testing.T,
	origin *httptest.Server,
) (*httpsproxy.Proxy, []string, string) {
	t.Helper()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatalf("parse origin URL: %v", err)
	}
	originAddress := originURL.Host
	targetAddress := net.JoinHostPort("runtime.unring.test", originURL.Port())
	upstream := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", originAddress)
		},
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // Isolated httptest origin only.
		},
	}
	authority, err := httpsproxy.EnsureAuthority(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureAuthority() error: %v", err)
	}
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		path := request.URL.Path
		response, err := upstream.RoundTrip(request)
		if err == nil && path == "/unframed" {
			// Go's transport presents decompressed HTTP/2 responses this way:
			// no Content-Length, no transfer encoding, and no connection close.
			// The proxy must add valid HTTP/1.1 framing for its TLS client.
			response.Proto = "HTTP/2.0"
			response.ProtoMajor = 2
			response.ProtoMinor = 0
			response.ContentLength = -1
			response.TransferEncoding = nil
			response.Close = false
			response.Uncompressed = true
			response.Header.Del("Content-Length")
			response.Header.Del("Transfer-Encoding")
		}
		return response, err
	})
	proxy, err := httpsproxy.Start(authority, httpsproxy.Options{
		Transport: transport,
		Approve: func(context.Context, httpsproxy.ApprovalRequest) (bool, error) {
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	t.Cleanup(func() {
		_ = proxy.Close()
	})
	environment, err := childenv.HTTPS(os.Environ(), proxy.Address(), authority.CertificatePath)
	if err != nil {
		t.Fatalf("build child environment: %v", err)
	}
	return proxy, environment, targetAddress
}

func runCurl(
	t *testing.T,
	curl string,
	environment []string,
	stdin []byte,
	arguments ...string,
) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	baseArguments := []string{
		"--silent", "--show-error", "--max-time", "4",
	}
	command := exec.CommandContext(ctx, curl, append(baseArguments, arguments...)...)
	command.Env = environment
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("curl did not exit within 5 seconds: %v\nstderr:\n%s",
			ctx.Err(), stderr.String())
	}
	if err != nil {
		t.Fatalf("curl exited nonzero: %v\nstderr:\n%s", err, stderr.String())
	}
	return stdout.Bytes()
}

func repeatedBody(pattern string, size int) []byte {
	body := make([]byte, 0, size)
	for len(body) < size {
		body = append(body, pattern...)
	}
	return body[:size]
}

func assertBytesEqual(t *testing.T, name string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs byte-for-byte: got %d bytes, want %d", name, len(got), len(want))
	}
}

func mapKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
