package httpsproxy_test

import (
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
	"strings"
	"testing"
	"time"

	"github.com/HongyuanJia8/unring/internal/childenv"
	"github.com/HongyuanJia8/unring/internal/httpsproxy"
)

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
	originURL, _ := url.Parse(origin.URL)
	originAddress := originURL.Host
	targetHost := "runtime.unring.test"
	targetAddress := net.JoinHostPort(targetHost, originURL.Port())

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
	proxy, err := httpsproxy.Start(authority, httpsproxy.Options{Transport: upstream})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	environment, err := childenv.HTTPS(os.Environ(), proxy.Address(), authority.CertificatePath)
	if err != nil {
		t.Fatalf("build child environment: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	curlCommand := exec.CommandContext(ctx, curl, "--silent", "--show-error",
		"https://"+targetAddress+"/curl")
	curlCommand.Env = environment
	curlOutput, err := curlCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("curl through unring: %v\n%s", err, curlOutput)
	}
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
	nodeCommand := exec.CommandContext(
		ctx, node, "-e", nodeScript, proxyHost, proxyPort, targetHost, originURL.Port(),
	)
	nodeCommand.Env = environment
	nodeOutput, err := nodeCommand.CombinedOutput()
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

func mapKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
