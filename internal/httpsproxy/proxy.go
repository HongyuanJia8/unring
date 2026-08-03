// Package httpsproxy implements unring's loopback HTTPS interception proxy.
package httpsproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hyj28/unring/internal/adapter"
)

type tlsCertificate = tls.Certificate

const maximumClassifiedBody = 16 << 20

const (
	// RequestDispositionSafeRead marks a forwarded request whose HTTP method is
	// safe. It is retained for auditability but does not require a final
	// commit/discard decision.
	RequestDispositionSafeRead = "safe-read"
	// RequestDispositionControlPlane marks an enumerated agent model request
	// that unring deliberately forwards without gating so the agent can run.
	RequestDispositionControlPlane = "agent-control-plane"
	// RequestDispositionApproved marks a mutation that was forwarded only
	// after the user approved it.
	RequestDispositionApproved = "approved"
)

// RequestRecord is one HTTPS request that was actually sent to an origin.
type RequestRecord struct {
	Method      string       `json:"method"`
	URL         string       `json:"url"`
	Tier        adapter.Tier `json:"tier,omitempty"`
	Adapter     string       `json:"adapter,omitempty"`
	Rule        string       `json:"rule,omitempty"`
	Disposition string       `json:"disposition,omitempty"`
	StatusCode  int          `json:"status_code,omitempty"`
	Error       string       `json:"error,omitempty"`
	StartedAt   time.Time    `json:"started_at"`
	EndedAt     time.Time    `json:"ended_at"`
	Undo        *UndoRecord  `json:"undo,omitempty"`
}

// UndoRecord is the declared and observed state of one compensating action.
// StillExists is shown even after success when compensation is inherently
// partial, such as closing rather than deleting a GitHub issue.
type UndoRecord struct {
	Method      string `json:"method,omitempty"`
	URL         string `json:"url,omitempty"`
	Effect      string `json:"effect"`
	StillExists string `json:"still_exists"`
	State       string `json:"state"`
	StatusCode  int    `json:"status_code,omitempty"`
	Error       string `json:"error,omitempty"`
}

// StagedRequest is a call withheld from the origin until the final decision.
type StagedRequest struct {
	Method           string      `json:"method"`
	URL              string      `json:"url"`
	Adapter          string      `json:"adapter"`
	Rule             string      `json:"rule"`
	IdempotencyKey   string      `json:"idempotency_key"`
	SyntheticStatus  int         `json:"synthetic_status"`
	State            string      `json:"state"`
	ReplayStatusCode int         `json:"replay_status_code,omitempty"`
	Error            string      `json:"error,omitempty"`
	Warning          string      `json:"warning,omitempty"`
	StagedAt         time.Time   `json:"staged_at"`
	Body             string      `json:"-"`
	Undo             *UndoRecord `json:"undo,omitempty"`
}

// ApprovalRecord is a needs-approval request and the user's decision.
type ApprovalRecord struct {
	Method   string    `json:"method"`
	URL      string    `json:"url"`
	Adapter  string    `json:"adapter,omitempty"`
	Rule     string    `json:"rule,omitempty"`
	Decision string    `json:"decision"`
	Error    string    `json:"error,omitempty"`
	Time     time.Time `json:"time"`
	Body     string    `json:"-"`
}

// ApprovalRequest is shown before a needs-approval call can reach its origin.
type ApprovalRequest struct {
	Method  string
	URL     string
	Adapter string
	Rule    string
	Reason  string
	Body    string
}

// UninterceptedItem is one connection unring saw but could not intercept.
type UninterceptedItem struct {
	Host   string    `json:"host"`
	Detail string    `json:"detail"`
	Time   time.Time `json:"time"`
}

// Summary is a point-in-time copy of HTTPS activity.
type Summary struct {
	Connections   int                 `json:"connections"`
	Requests      []RequestRecord     `json:"requests"`
	Staged        []StagedRequest     `json:"staged"`
	Approvals     []ApprovalRecord    `json:"approvals"`
	Unintercepted []UninterceptedItem `json:"unintercepted"`
	Sealed        bool                `json:"sealed"`
	Finalized     bool                `json:"finalized"`
}

// HasReviewableActivity reports whether HTTPS activity requires a final
// decision or a coverage warning.
func (summary Summary) HasReviewableActivity() bool {
	return !summary.Sealed || summary.HasForwardedEffects() || len(summary.Staged) > 0 ||
		len(summary.Approvals) > 0 || len(summary.Unintercepted) > 0
}

// HasForwardedEffects reports whether a forwarded HTTPS request may have
// changed external state. Safe reads and enumerated agent control-plane calls
// do not manufacture a final decision or reversibility warning; callers still
// display them as observed traffic.
func (summary Summary) HasForwardedEffects() bool {
	for _, request := range summary.Requests {
		if request.Disposition != RequestDispositionSafeRead &&
			request.Disposition != RequestDispositionControlPlane {
			return true
		}
	}
	return false
}

// Options configures upstream forwarding and explicit CONNECT passthrough.
type Options struct {
	Transport         http.RoundTripper
	PassthroughHost   func(host string) bool
	AgentControlPlane func(*http.Request) bool
	Adapters          *adapter.Set
	Approve           func(context.Context, ApprovalRequest) (bool, error)
	StagedUpdated     func(Summary) error
}

// Proxy intercepts HTTPS CONNECT connections on loopback.
type Proxy struct {
	authority     *Authority
	listener      net.Listener
	server        *http.Server
	transport     http.RoundTripper
	passthrough   func(string) bool
	controlPlane  func(*http.Request) bool
	adapters      *adapter.Set
	approve       func(context.Context, ApprovalRequest) (bool, error)
	stagedUpdated func(Summary) error
	runCtx        context.Context
	cancel        context.CancelFunc

	connectionsMu sync.Mutex
	connections   map[net.Conn]struct{}
	connectionWG  sync.WaitGroup
	handlerWG     sync.WaitGroup
	sealing       bool

	summaryMu sync.Mutex
	summary   Summary
	staged    []stagedCall
	undoCalls []undoCall

	closeOnce sync.Once
	closeErr  error

	finalizeOnce sync.Once
	finalizeErr  error
}

type stagedCall struct {
	method string
	url    *url.URL
	host   string
	header http.Header
	body   []byte
	key    string
	undo   *adapter.Undo
	input  adapter.Request
}

type undoCall struct {
	method string
	url    string
	header http.Header
	body   []byte
	target string
	index  int
}

// Start binds an HTTPS proxy to an ephemeral loopback port.
func Start(authority *Authority, options Options) (*Proxy, error) {
	if authority == nil || authority.Certificate == nil || authority.PrivateKey == nil {
		return nil, errors.New("start HTTPS proxy: missing certificate authority")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for HTTPS proxy clients: %w", err)
	}
	transport := options.Transport
	if transport == nil {
		rootCAs, err := configuredRootCAs(os.Getenv("SSL_CERT_FILE"))
		if err != nil {
			_ = listener.Close()
			return nil, fmt.Errorf("load HTTPS upstream roots: %w", err)
		}
		transport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     false,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			TLSClientConfig:       &tls.Config{RootCAs: rootCAs, MinVersion: tls.VersionTLS12},
		}
	}
	proxy := &Proxy{
		authority: authority, listener: listener, transport: transport,
		passthrough: options.PassthroughHost, controlPlane: options.AgentControlPlane,
		adapters: options.Adapters,
		approve:  options.Approve, stagedUpdated: options.StagedUpdated,
		connections: make(map[net.Conn]struct{}),
	}
	proxy.runCtx, proxy.cancel = context.WithCancel(context.Background())
	proxy.server = &http.Server{
		Handler:           http.HandlerFunc(proxy.serveHTTP),
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() {
		err := proxy.server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			proxy.recordUnintercepted("", "HTTPS proxy listener failed: "+err.Error())
		}
	}()
	return proxy, nil
}

func configuredRootCAs(additionalBundle string) (*x509.CertPool, error) {
	if strings.TrimSpace(additionalBundle) == "" {
		return nil, nil
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("load system certificate roots: %w", err)
	}
	pemBytes, err := os.ReadFile(additionalBundle)
	if err != nil {
		return nil, fmt.Errorf("read SSL_CERT_FILE %s: %w", additionalBundle, err)
	}
	if !roots.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("SSL_CERT_FILE %s contains no certificates", additionalBundle)
	}
	return roots, nil
}

// Address returns the loopback address of the proxy.
func (proxy *Proxy) Address() string {
	return proxy.listener.Addr().String()
}

// Summary returns a detached activity snapshot.
func (proxy *Proxy) Summary() Summary {
	proxy.summaryMu.Lock()
	defer proxy.summaryMu.Unlock()
	summary := Summary{
		Connections:   proxy.summary.Connections,
		Requests:      append([]RequestRecord(nil), proxy.summary.Requests...),
		Staged:        append([]StagedRequest(nil), proxy.summary.Staged...),
		Approvals:     append([]ApprovalRecord(nil), proxy.summary.Approvals...),
		Unintercepted: append([]UninterceptedItem(nil), proxy.summary.Unintercepted...),
		Sealed:        proxy.summary.Sealed,
		Finalized:     proxy.summary.Finalized,
	}
	for index := range summary.Requests {
		summary.Requests[index].Undo = cloneUndoRecord(summary.Requests[index].Undo)
	}
	for index := range summary.Staged {
		summary.Staged[index].Undo = cloneUndoRecord(summary.Staged[index].Undo)
	}
	return summary
}

// Seal stops new and active proxy connections and freezes the summary.
func (proxy *Proxy) Seal(ctx context.Context) error {
	proxy.closeOnce.Do(func() {
		proxy.cancel()
		proxy.connectionsMu.Lock()
		proxy.sealing = true
		proxy.connectionsMu.Unlock()
		proxy.closeErr = proxy.server.Close()
		proxy.connectionsMu.Lock()
		for connection := range proxy.connections {
			_ = connection.Close()
		}
		proxy.connectionsMu.Unlock()

		done := make(chan struct{})
		go func() {
			proxy.handlerWG.Wait()
			proxy.connectionWG.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			proxy.closeErr = errors.Join(proxy.closeErr, ctx.Err())
		}
		if closer, ok := proxy.transport.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
		proxy.summaryMu.Lock()
		proxy.summary.Sealed = true
		proxy.summaryMu.Unlock()
	})
	if errors.Is(proxy.closeErr, http.ErrServerClosed) || errors.Is(proxy.closeErr, net.ErrClosed) {
		return nil
	}
	return proxy.closeErr
}

// Finalize sends all staged calls on commit or drops them on discard. The
// operation is single-shot because one decision applies to the whole session.
func (proxy *Proxy) Finalize(ctx context.Context, commit bool) error {
	proxy.finalizeOnce.Do(func() {
		if !proxy.Summary().Sealed {
			proxy.finalizeErr = errors.New("finalize HTTPS proxy: proxy is not sealed")
			return
		}
		proxy.summaryMu.Lock()
		calls := append([]stagedCall(nil), proxy.staged...)
		if !commit {
			for index := range proxy.summary.Staged {
				proxy.summary.Staged[index].State = "discarded"
			}
			proxy.staged = nil
			proxy.summaryMu.Unlock()
			var discardErrors []error
			if err := proxy.persistStagedUpdate(); err != nil {
				discardErrors = append(discardErrors,
					fmt.Errorf("record discarded staged requests: %w", err))
			}
			discardErrors = append(discardErrors, proxy.runUndos(ctx)...)
			proxy.summaryMu.Lock()
			proxy.summary.Finalized = true
			proxy.summaryMu.Unlock()
			if err := proxy.persistStagedUpdate(); err != nil {
				discardErrors = append(discardErrors,
					fmt.Errorf("record discard compensation outcomes: %w", err))
			}
			proxy.finalizeErr = errors.Join(discardErrors...)
			return
		}
		proxy.summaryMu.Unlock()

		var replayErrors []error
		for index, call := range calls {
			proxy.summaryMu.Lock()
			if index < len(proxy.summary.Staged) {
				proxy.summary.Staged[index].State = "sending"
				proxy.summary.Staged[index].Error = ""
				proxy.summary.Staged[index].Warning = ""
			}
			proxy.summaryMu.Unlock()
			if err := proxy.persistStagedUpdate(); err != nil {
				proxy.summaryMu.Lock()
				if index < len(proxy.summary.Staged) {
					proxy.summary.Staged[index].State = "not-sent"
					proxy.summary.Staged[index].Error = "persist sending state: " + err.Error()
				}
				proxy.summaryMu.Unlock()
				replayErrors = append(replayErrors, fmt.Errorf(
					"record staged request %s %s before sending: %w", call.method, call.url, err,
				))
				continue
			}

			result := proxy.replay(ctx, call)
			proxy.summaryMu.Lock()
			if index < len(proxy.summary.Staged) {
				proxy.summary.Staged[index].ReplayStatusCode = result.status
				proxy.summary.Staged[index].State = result.state
				proxy.summary.Staged[index].Warning = result.warning
				proxy.summary.Staged[index].Error = errorText(result.err)
				if result.undoRecord != nil {
					proxy.summary.Staged[index].Undo = result.undoRecord
				}
			}
			proxy.summaryMu.Unlock()
			if result.undo != nil || result.undoRecord != nil {
				if result.undo == nil {
					result.undo = &undoCall{}
				}
				result.undo.target = "staged"
				result.undo.index = index
				proxy.summaryMu.Lock()
				proxy.undoCalls = append(proxy.undoCalls, *result.undo)
				proxy.summaryMu.Unlock()
			}
			if persistErr := proxy.persistStagedUpdate(); persistErr != nil {
				replayErrors = append(replayErrors, fmt.Errorf(
					"record staged request %s %s outcome: %w", call.method, call.url, persistErr,
				))
			}
			if result.err != nil {
				replayErrors = append(replayErrors, fmt.Errorf(
					"send staged request %s %s: %w", call.method, call.url, result.err,
				))
			}
		}
		proxy.summaryMu.Lock()
		proxy.staged = nil
		proxy.summaryMu.Unlock()
		proxy.summaryMu.Lock()
		proxy.summary.Finalized = true
		proxy.summaryMu.Unlock()
		if err := proxy.persistStagedUpdate(); err != nil {
			replayErrors = append(replayErrors, fmt.Errorf("record finalized staged requests: %w", err))
		}
		proxy.finalizeErr = errors.Join(replayErrors...)
	})
	return proxy.finalizeErr
}

type replayResult struct {
	status     int
	state      string
	warning    string
	err        error
	undo       *undoCall
	undoRecord *UndoRecord
}

func (proxy *Proxy) replay(ctx context.Context, call stagedCall) replayResult {
	// Supplying an opaque ReadCloser deliberately leaves GetBody nil, even for
	// an empty body. Together with a non-NoBody Body this prevents net/http from
	// treating our Idempotency-Key header as permission to retry a write after
	// the request may already have reached the origin.
	body := io.NopCloser(bytes.NewReader(call.body))
	request, err := http.NewRequestWithContext(ctx, call.method, call.url.String(), body)
	if err != nil {
		return replayResult{state: "not-sent", err: fmt.Errorf("build replay request: %w", err)}
	}
	request.ContentLength = int64(len(call.body))
	request.GetBody = nil
	request.Host = call.host
	request.Header = call.header.Clone()
	request.Header.Set("Idempotency-Key", call.key)
	removeHopByHopHeaders(request.Header)
	response, err := proxy.transport.RoundTrip(request)
	if err != nil {
		return replayResult{state: "unknown", err: err}
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, maximumClassifiedBody+1))
	if response.StatusCode >= 200 && response.StatusCode <= 299 {
		result := replayResult{status: response.StatusCode, state: "sent"}
		if readErr != nil {
			result.warning = fmt.Sprintf(
				"origin accepted the request with HTTP %d, but reading its response body failed: %v",
				response.StatusCode, readErr,
			)
		}
		if call.undo != nil {
			record, undo := proxy.prepareUndo(call.undo, call.input, responseBody)
			result.undoRecord = record
			result.undo = undo
			if undo == nil {
				result.warning = joinWarning(result.warning,
					"discard compensation unavailable: "+record.Error)
			}
		}
		return result
	}
	err = fmt.Errorf("origin returned HTTP %d; delivery outcome is unknown", response.StatusCode)
	if readErr != nil {
		err = errors.Join(err, fmt.Errorf("read replay response: %w", readErr))
	}
	return replayResult{status: response.StatusCode, state: "unknown", err: err}
}

func (proxy *Proxy) persistStagedUpdate() error {
	if proxy.stagedUpdated == nil {
		return nil
	}
	return proxy.stagedUpdated(proxy.Summary())
}

// Close seals and safely discards any staged calls with bounded timeouts.
func (proxy *Proxy) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(proxy.Seal(ctx), proxy.Finalize(ctx, false))
}

func (proxy *Proxy) serveHTTP(response http.ResponseWriter, request *http.Request) {
	finish, accepted := proxy.beginHandler()
	if !accepted {
		return
	}
	defer finish()
	if request.Method != http.MethodConnect {
		proxy.recordUnintercepted(request.Host,
			"plain HTTP reached unring and was blocked; it was not forwarded or intercepted")
		http.Error(response,
			"unring blocked plain HTTP because this slice cannot safely intercept it",
			http.StatusBadRequest)
		return
	}
	hostport := request.Host
	if _, _, err := net.SplitHostPort(hostport); err != nil {
		hostport = net.JoinHostPort(hostport, "443")
	}
	proxy.summaryMu.Lock()
	proxy.summary.Connections++
	proxy.summaryMu.Unlock()
	if proxy.passthrough != nil && proxy.passthrough(request.Host) {
		proxy.tunnel(response, hostport)
		return
	}
	proxy.intercept(response, hostport)
}

func (proxy *Proxy) beginHandler() (func(), bool) {
	proxy.connectionsMu.Lock()
	defer proxy.connectionsMu.Unlock()
	if proxy.sealing {
		return func() {}, false
	}
	proxy.handlerWG.Add(1)
	return proxy.handlerWG.Done, true
}

func (proxy *Proxy) track(connection net.Conn) (func(), bool) {
	proxy.connectionsMu.Lock()
	if proxy.sealing {
		proxy.connectionsMu.Unlock()
		_ = connection.Close()
		return func() {}, false
	}
	proxy.connections[connection] = struct{}{}
	proxy.connectionWG.Add(1)
	proxy.connectionsMu.Unlock()
	return func() {
		proxy.connectionsMu.Lock()
		delete(proxy.connections, connection)
		proxy.connectionsMu.Unlock()
		proxy.connectionWG.Done()
		_ = connection.Close()
	}, true
}

func (proxy *Proxy) tunnel(response http.ResponseWriter, hostport string) {
	client, buffered, err := hijack(response)
	if err != nil {
		proxy.recordUnintercepted(hostport, "CONNECT passthrough could not take ownership: "+err.Error())
		return
	}
	cleanup, tracked := proxy.track(client)
	if !tracked {
		return
	}
	defer cleanup()
	dialContext, cancel := context.WithTimeout(proxy.runCtx, 10*time.Second)
	defer cancel()
	upstream, err := dialCONNECTTarget(dialContext, hostport)
	if err != nil {
		proxy.recordUnintercepted(hostport, "CONNECT tunnel was passed through but dialing failed: "+err.Error())
		_, _ = buffered.WriteString("HTTP/1.1 502 Bad Gateway\r\n\r\n")
		_ = buffered.Flush()
		return
	}
	defer upstream.Close()
	proxy.recordUnintercepted(hostport,
		"CONNECT tunnel was passed through without TLS interception; request contents are unknown")
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}
	copyDone := make(chan struct{}, 1)
	go func() {
		_, _ = io.Copy(upstream, client)
		if closeWriter, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = closeWriter.CloseWrite()
		}
		copyDone <- struct{}{}
	}()
	_, _ = io.Copy(client, upstream)
	<-copyDone
}

func dialCONNECTTarget(ctx context.Context, hostport string) (net.Conn, error) {
	target := &http.Request{URL: &url.URL{Scheme: "https", Host: hostport}}
	upstreamProxy, err := http.ProxyFromEnvironment(target)
	if err != nil {
		return nil, fmt.Errorf("resolve inherited upstream proxy: %w", err)
	}
	if upstreamProxy == nil {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", hostport)
	}
	if upstreamProxy.Scheme != "http" && upstreamProxy.Scheme != "https" {
		return nil, fmt.Errorf(
			"inherited upstream proxy scheme %q is unsupported for CONNECT passthrough",
			upstreamProxy.Scheme,
		)
	}
	proxyAddress := upstreamProxy.Host
	if _, _, err := net.SplitHostPort(proxyAddress); err != nil {
		defaultPort := "80"
		if upstreamProxy.Scheme == "https" {
			defaultPort = "443"
		}
		proxyAddress = net.JoinHostPort(upstreamProxy.Hostname(), defaultPort)
	}
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, fmt.Errorf("dial inherited upstream proxy: %w", err)
	}
	closeConnection := true
	defer func() {
		if closeConnection {
			_ = connection.Close()
		}
	}()
	if upstreamProxy.Scheme == "https" {
		secure := tls.Client(connection, &tls.Config{
			ServerName: upstreamProxy.Hostname(), MinVersion: tls.VersionTLS12,
		})
		if err := secure.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("handshake with inherited upstream proxy: %w", err)
		}
		connection = secure
	}
	connectRequest := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: hostport},
		Host:   hostport,
		Header: make(http.Header),
	}
	if upstreamProxy.User != nil {
		password, _ := upstreamProxy.User.Password()
		connectRequest.SetBasicAuth(upstreamProxy.User.Username(), password)
		connectRequest.Header.Set(
			"Proxy-Authorization", connectRequest.Header.Get("Authorization"),
		)
		connectRequest.Header.Del("Authorization")
	}
	if err := connectRequest.Write(connection); err != nil {
		return nil, fmt.Errorf("write CONNECT to inherited upstream proxy: %w", err)
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, connectRequest)
	if err != nil {
		return nil, fmt.Errorf("read CONNECT from inherited upstream proxy: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(
			"inherited upstream proxy refused CONNECT with HTTP %d",
			response.StatusCode,
		)
	}
	closeConnection = false
	return &bufferedConn{Conn: connection, reader: reader}, nil
}

type bufferedConn struct {
	net.Conn
	reader io.Reader
}

func (connection *bufferedConn) Read(buffer []byte) (int, error) {
	return connection.reader.Read(buffer)
}

func (connection *bufferedConn) CloseWrite() error {
	if closeWriter, ok := connection.Conn.(interface{ CloseWrite() error }); ok {
		return closeWriter.CloseWrite()
	}
	return nil
}

func (proxy *Proxy) intercept(response http.ResponseWriter, hostport string) {
	client, buffered, err := hijack(response)
	if err != nil {
		proxy.recordUnintercepted(hostport, "HTTPS CONNECT could not be intercepted: "+err.Error())
		return
	}
	cleanup, tracked := proxy.track(client)
	if !tracked {
		return
	}
	defer cleanup()
	certificate, err := proxy.authority.certificateForHost(hostport)
	if err != nil {
		proxy.recordUnintercepted(hostport, "HTTPS certificate creation failed: "+err.Error())
		return
	}
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		proxy.recordUnintercepted(hostport, "HTTPS CONNECT response failed: "+err.Error())
		return
	}
	if err := buffered.Flush(); err != nil {
		proxy.recordUnintercepted(hostport, "HTTPS CONNECT response failed: "+err.Error())
		return
	}
	tlsClient := tls.Server(client, &tls.Config{
		Certificates: []tls.Certificate{*certificate},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	})
	_ = tlsClient.SetDeadline(time.Now().Add(15 * time.Second))
	if err := tlsClient.Handshake(); err != nil {
		proxy.recordUnintercepted(hostport,
			"TLS handshake failed; the client may not trust unring's per-process CA: "+err.Error())
		return
	}
	_ = tlsClient.SetDeadline(time.Time{})
	reader := bufio.NewReader(tlsClient)
	for {
		request, err := http.ReadRequest(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) &&
				!errors.Is(err, io.ErrUnexpectedEOF) {
				proxy.recordUnintercepted(hostport,
					"intercepted TLS stream could not be decoded as HTTP: "+err.Error())
			}
			return
		}
		closeConnection, err := proxy.forward(tlsClient, reader, request, hostport)
		if err != nil {
			return
		}
		if closeConnection {
			return
		}
	}
}

func (proxy *Proxy) forward(
	client io.ReadWriteCloser,
	clientReader io.Reader,
	request *http.Request,
	hostport string,
) (bool, error) {
	body, err := io.ReadAll(io.LimitReader(request.Body, maximumClassifiedBody+1))
	if request.Body != nil {
		_ = request.Body.Close()
	}
	if err != nil {
		proxy.recordUnintercepted(hostport,
			"intercepted request body could not be read and was blocked: "+err.Error())
		writeProxyResponse(client, http.StatusBadRequest, http.Header{
			"X-Unring-Blocked": []string{"true"},
		}, "")
		return true, err
	}
	if len(body) > maximumClassifiedBody {
		detail := fmt.Sprintf(
			"intercepted request body exceeded the %d byte classification limit and was blocked",
			maximumClassifiedBody,
		)
		proxy.recordUnintercepted(hostport, detail)
		writeProxyResponse(client, http.StatusRequestEntityTooLarge, http.Header{
			"X-Unring-Blocked": []string{"true"},
		}, "")
		return true, errors.New(detail)
	}

	request.RequestURI = ""
	request.URL.Scheme = "https"
	request.URL.Host = hostport
	if request.Host == "" {
		request.Host = hostport
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))

	classification := adapter.Classification{}
	disposition := ""
	matched := false
	if proxy.controlPlane != nil && proxy.controlPlane(request) {
		classification = adapter.Classification{
			Tier: adapter.TierAlreadyIrreversible,
			Rule: RequestDispositionControlPlane,
		}
		disposition = RequestDispositionControlPlane
		matched = true
	}
	if !matched {
		classification, matched, err = proxy.classify(request, body)
	}
	if err != nil {
		proxy.recordUnintercepted(hostport,
			"adapter classification failed and the request was blocked: "+err.Error())
		writeProxyResponse(client, http.StatusBadGateway, http.Header{
			"X-Unring-Blocked": []string{"true"},
		}, "")
		return true, err
	}
	if !matched && safeHTTPMethod(request.Method) {
		classification = adapter.Classification{
			Tier: adapter.TierAlreadyIrreversible, Rule: "safe-http-method",
		}
		disposition = RequestDispositionSafeRead
		matched = true
	}
	if !matched {
		classification = adapter.Classification{Tier: adapter.TierNeedsApproval}
	}

	switch classification.Tier {
	case adapter.TierStageable:
		return proxy.stage(client, request, body, classification)
	case adapter.TierNeedsApproval:
		return proxy.requestApproval(
			client, clientReader, request, body, hostport, classification,
		)
	case adapter.TierAlreadyIrreversible:
		return proxy.forwardActual(
			client, clientReader, request, body, classification, disposition,
		)
	default:
		err := fmt.Errorf("classification returned unsupported tier %q", classification.Tier)
		proxy.recordUnintercepted(hostport, err.Error()+"; request was blocked")
		writeProxyResponse(client, http.StatusBadGateway, http.Header{
			"X-Unring-Blocked": []string{"true"},
		}, "")
		return true, err
	}
}

func (proxy *Proxy) classify(
	request *http.Request,
	body []byte,
) (adapter.Classification, bool, error) {
	if proxy.adapters == nil {
		return adapter.Classification{}, false, nil
	}
	return proxy.adapters.Classify(adapter.Request{
		Method: request.Method, URL: request.URL, Header: request.Header, Body: body,
	})
}

func safeHTTPMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func (proxy *Proxy) stage(
	client io.Writer,
	request *http.Request,
	body []byte,
	classification adapter.Classification,
) (bool, error) {
	if classification.Response == nil {
		err := errors.New("stageable classification has no synthesized response")
		proxy.recordUnintercepted(request.URL.Host, err.Error()+"; request was blocked")
		writeProxyResponse(client, http.StatusBadGateway, http.Header{
			"X-Unring-Blocked": []string{"true"},
		}, "")
		return true, err
	}
	headers := make(http.Header, len(classification.Response.Headers))
	for name, value := range classification.Response.Headers {
		headers.Set(name, value)
	}
	// This protocol marker belongs to unring, not the adapter. Apply it last so
	// case canonicalization or a contradictory map key can never clear it.
	headers.Set("X-Unring-Staged", "true")
	started := time.Now().UTC()
	call := stagedCall{
		method: request.Method,
		url:    cloneURL(request.URL),
		host:   request.Host,
		header: request.Header.Clone(),
		body:   append([]byte(nil), body...),
		key:    classification.IdempotencyKey,
		undo:   classification.Undo,
		input: adapter.Request{
			Method: request.Method, URL: cloneURL(request.URL),
			Header: request.Header.Clone(), Body: append([]byte(nil), body...),
		},
	}
	var undoDisclosure *UndoRecord
	if classification.Undo != nil {
		undoDisclosure = &UndoRecord{
			Method: classification.Undo.Method, URL: classification.Undo.URL,
			Effect:      undoEffect(classification.Undo),
			StillExists: undoStillExists(classification.Undo),
			State:       "not-needed",
		}
	}
	proxy.summaryMu.Lock()
	proxy.staged = append(proxy.staged, call)
	proxy.summary.Staged = append(proxy.summary.Staged, StagedRequest{
		Method: request.Method, URL: request.URL.String(),
		Adapter: classification.Adapter, Rule: classification.Rule,
		IdempotencyKey:  classification.IdempotencyKey,
		SyntheticStatus: classification.Response.Status,
		State:           "pending", StagedAt: started, Body: string(body),
		Undo: undoDisclosure,
	})
	proxy.summaryMu.Unlock()
	if err := writeProxyResponse(
		client, classification.Response.Status, headers, classification.Response.Body,
	); err != nil {
		return true, err
	}
	return request.Close, nil
}

func (proxy *Proxy) requestApproval(
	client io.ReadWriteCloser,
	clientReader io.Reader,
	request *http.Request,
	body []byte,
	hostport string,
	classification adapter.Classification,
) (bool, error) {
	reason := "no adapter or safe HTTP heuristic recognized this request"
	if classification.Adapter != "" {
		reason = fmt.Sprintf(
			"adapter %s rule %s classifies this call as needs approval",
			classification.Adapter, classification.Rule,
		)
	}
	approval := ApprovalRequest{
		Method: request.Method, URL: request.URL.String(),
		Adapter: classification.Adapter, Rule: classification.Rule,
		Reason: reason, Body: string(body),
	}
	approved := false
	var approvalErr error
	if proxy.approve != nil {
		approved, approvalErr = proxy.approve(proxy.runCtx, approval)
	}
	decision := "declined"
	if approved {
		decision = "approved"
	}
	if approvalErr != nil {
		decision = "error"
	}
	proxy.summaryMu.Lock()
	proxy.summary.Approvals = append(proxy.summary.Approvals, ApprovalRecord{
		Method: request.Method, URL: request.URL.String(),
		Adapter: classification.Adapter, Rule: classification.Rule,
		Decision: decision, Error: errorText(approvalErr),
		Time: time.Now().UTC(), Body: string(body),
	})
	proxy.summaryMu.Unlock()
	if approvalErr != nil {
		proxy.recordUnintercepted(hostport,
			"needs-approval prompt failed and the request was blocked: "+approvalErr.Error())
		writeProxyResponse(client, http.StatusBadGateway, http.Header{
			"X-Unring-Approval": []string{"error"},
		}, "")
		return true, approvalErr
	}
	if !approved {
		err := writeProxyResponse(client, http.StatusForbidden, http.Header{
			"Content-Type":      []string{"application/json"},
			"X-Unring-Approval": []string{"declined"},
		}, `{"error":"unring approval declined","sent":false}`)
		return request.Close, err
	}
	return proxy.forwardActual(
		client, clientReader, request, body, classification, RequestDispositionApproved,
	)
}

func (proxy *Proxy) forwardActual(
	client io.ReadWriteCloser,
	clientReader io.Reader,
	request *http.Request,
	requestBody []byte,
	classification adapter.Classification,
	disposition string,
) (bool, error) {
	started := time.Now().UTC()
	record := RequestRecord{
		Method: request.Method, URL: request.URL.String(),
		Tier: classification.Tier, Adapter: classification.Adapter,
		Rule: classification.Rule, Disposition: disposition,
		StartedAt: started,
	}
	undoInput := adapter.Request{
		Method: request.Method, URL: cloneURL(request.URL),
		Header: request.Header.Clone(), Body: append([]byte(nil), requestBody...),
	}
	request = request.WithContext(proxy.runCtx)
	requestedUpgrade := protocolUpgrade(request.Header)
	if requestedUpgrade == "" {
		removeHopByHopHeaders(request.Header)
	} else {
		prepareUpgradeHeaders(request.Header, requestedUpgrade)
	}
	if request.Body != nil {
		defer request.Body.Close()
	}
	response, err := proxy.transport.RoundTrip(request)
	if err != nil {
		record.Error = forwardingError("upstream request", err)
		record.EndedAt = time.Now().UTC()
		proxy.recordRequest(record)
		_, _ = io.WriteString(client,
			"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return true, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusSwitchingProtocols {
		var controlPlaneRecord *RequestRecord
		if record.Disposition == RequestDispositionControlPlane {
			controlPlaneRecord = &record
		}
		return proxy.forwardUpgrade(
			client, clientReader, request, response, request.URL.Host, requestedUpgrade,
			controlPlaneRecord,
		)
	}
	if requestedUpgrade != "" {
		// A proper upgrade client will not send frames after a non-101
		// response. Close anyway so a broken client cannot feed binary data
		// into the next HTTP request decoder.
		request.Close = true
	}
	record.StatusCode = response.StatusCode
	removeHopByHopHeaders(response.Header)
	closeClient := prepareClientResponse(response, request)
	var capture boundedCapture
	response.Body = io.NopCloser(io.TeeReader(response.Body, &capture))
	if err := response.Write(client); err != nil {
		record.Error = forwardingError("response body relay", err)
		record.EndedAt = time.Now().UTC()
		if record.StatusCode >= 200 && record.StatusCode <= 299 {
			proxy.recordRequestWithUndo(record, classification.Undo, undoInput, capture.Bytes())
		} else {
			proxy.recordRequest(record)
		}
		return true, err
	}
	record.EndedAt = time.Now().UTC()
	if record.StatusCode >= 200 && record.StatusCode <= 299 {
		proxy.recordRequestWithUndo(record, classification.Undo, undoInput, capture.Bytes())
	} else {
		proxy.recordRequest(record)
	}
	return closeClient, nil
}

func forwardingError(phase string, err error) string {
	var networkError net.Error
	var dnsError *net.DNSError
	switch {
	case errors.As(err, &networkError) && networkError.Timeout():
		return "timeout during " + phase + ": " + err.Error()
	case errors.As(err, &dnsError):
		return "DNS failure during " + phase + ": " + err.Error()
	case isTLSFailure(err):
		return "TLS failure during " + phase + ": " + err.Error()
	default:
		return phase + " failed: " + err.Error()
	}
}

func isTLSFailure(err error) bool {
	var recordHeader tls.RecordHeaderError
	var verification *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalidCertificate x509.CertificateInvalidError
	var systemRoots x509.SystemRootsError
	var insecureAlgorithm x509.InsecureAlgorithmError
	var constraintViolation x509.ConstraintViolationError
	return errors.As(err, &recordHeader) ||
		errors.As(err, &verification) ||
		errors.As(err, &unknownAuthority) ||
		errors.As(err, &hostname) ||
		errors.As(err, &invalidCertificate) ||
		errors.As(err, &systemRoots) ||
		errors.As(err, &insecureAlgorithm) ||
		errors.As(err, &constraintViolation) ||
		hasTLSLeafError(err)
}

// hasTLSLeafError recognizes crypto/tls errors whose concrete types are not
// exported, including peer alerts, plus negotiation errors returned as plain
// errors. It deliberately inspects only leaves: wrapper messages such as
// url.Error include the request URL and must not influence classification.
func hasTLSLeafError(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if hasTLSLeafError(child) {
				return true
			}
		}
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		if child := wrapped.Unwrap(); child != nil {
			return hasTLSLeafError(child)
		}
	}
	return strings.HasPrefix(err.Error(), "tls: ")
}

func writeProxyResponse(
	writer io.Writer,
	status int,
	headers http.Header,
	body string,
) error {
	response := &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: headers.Clone(), Body: io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	return response.Write(writer)
}

func cloneURL(source *url.URL) *url.URL {
	clone := *source
	return &clone
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (proxy *Proxy) forwardUpgrade(
	client io.ReadWriteCloser,
	clientReader io.Reader,
	request *http.Request,
	response *http.Response,
	hostport string,
	requestedUpgrade string,
	controlPlaneRecord *RequestRecord,
) (bool, error) {
	responseUpgrade := protocolUpgrade(response.Header)
	if requestedUpgrade == "" || responseUpgrade == "" ||
		!strings.EqualFold(requestedUpgrade, responseUpgrade) {
		proxy.recordUnintercepted(hostport,
			"an invalid or unsolicited HTTP protocol upgrade was blocked")
		_, _ = io.WriteString(client,
			"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return true, errors.New("block invalid HTTP protocol upgrade")
	}
	upstream, ok := response.Body.(io.ReadWriteCloser)
	if !ok {
		proxy.recordUnintercepted(hostport,
			"HTTP protocol upgrade could not be tunneled because the upstream was not bidirectional")
		_, _ = io.WriteString(client,
			"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return true, errors.New("upstream protocol upgrade is not bidirectional")
	}

	handshake := new(http.Response)
	*handshake = *response
	handshake.Header = response.Header.Clone()
	handshake.Body = http.NoBody
	handshake.ContentLength = 0
	handshake.TransferEncoding = nil
	handshake.Request = request
	handshake.Proto = "HTTP/1.1"
	handshake.ProtoMajor = 1
	handshake.ProtoMinor = 1
	prepareUpgradeHeaders(handshake.Header, responseUpgrade)
	if err := handshake.Write(client); err != nil {
		return true, fmt.Errorf("write HTTP protocol upgrade response: %w", err)
	}

	if controlPlaneRecord != nil {
		controlPlaneRecord.StatusCode = response.StatusCode
		controlPlaneRecord.EndedAt = time.Now().UTC()
		proxy.recordRequest(*controlPlaneRecord)
	} else {
		proxy.recordUnintercepted(hostport, fmt.Sprintf(
			"HTTP protocol upgrade to %q was tunneled without payload interception",
			responseUpgrade,
		))
	}
	return true, relayUpgrade(client, clientReader, upstream)
}

func relayUpgrade(
	client io.ReadWriteCloser,
	clientReader io.Reader,
	upstream io.ReadWriteCloser,
) error {
	results := make(chan error, 2)
	go func() {
		_, err := io.Copy(upstream, clientReader)
		results <- err
	}()
	go func() {
		_, err := io.Copy(client, upstream)
		results <- err
	}()
	first := <-results
	_ = upstream.Close()
	_ = client.Close()
	second := <-results
	return errors.Join(ignoreClosedConnection(first), ignoreClosedConnection(second))
}

func ignoreClosedConnection(err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// prepareClientResponse translates the upstream response into the HTTP/1.1
// protocol negotiated inside the CONNECT tunnel. In particular, Go exposes
// decompressed HTTP/2 responses with neither Content-Length nor a transfer
// encoding. Response.Write would emit such a body without any delimiter, so
// the client would wait forever for bytes that will never arrive.
func prepareClientResponse(response *http.Response, request *http.Request) bool {
	closeClient := request.Close || response.Close
	response.Request = request
	response.Proto = "HTTP/1.1"
	response.ProtoMajor = 1
	response.ProtoMinor = 1
	response.Close = closeClient
	response.Uncompressed = false

	bodyExpected := request.Method != http.MethodHead &&
		response.StatusCode >= 200 &&
		response.StatusCode != http.StatusNoContent &&
		response.StatusCode != http.StatusNotModified
	switch {
	case !bodyExpected:
		response.TransferEncoding = nil
	case len(response.Trailer) > 0:
		response.ContentLength = -1
		response.TransferEncoding = []string{"chunked"}
	case response.ContentLength >= 0:
		response.TransferEncoding = nil
	case !closeClient:
		// Re-frame the already decoded upstream body for HTTP/1.1. The
		// terminating zero-size chunk tells a keep-alive client the response
		// is complete without buffering the body to discover its length.
		response.TransferEncoding = []string{"chunked"}
	default:
		// Response.Write emits Connection: close; intercept must honor that
		// choice by returning closeClient to its caller.
		response.TransferEncoding = nil
	}
	return closeClient
}

func removeHopByHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{
		"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		header.Del(name)
	}
}

func protocolUpgrade(header http.Header) string {
	if !headerHasToken(header, "Connection", "upgrade") {
		return ""
	}
	return strings.TrimSpace(header.Get("Upgrade"))
}

func prepareUpgradeHeaders(header http.Header, upgrade string) {
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			name = strings.TrimSpace(name)
			if !strings.EqualFold(name, "upgrade") {
				header.Del(name)
			}
		}
	}
	for _, name := range []string{
		"Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding",
	} {
		header.Del(name)
	}
	header.Set("Connection", "Upgrade")
	header.Set("Upgrade", upgrade)
}

func headerHasToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func hijack(response http.ResponseWriter) (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := response.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("HTTP server does not support connection hijacking")
	}
	connection, buffered, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	return connection, buffered, nil
}

func (proxy *Proxy) recordRequest(record RequestRecord) {
	proxy.summaryMu.Lock()
	proxy.summary.Requests = append(proxy.summary.Requests, record)
	proxy.summaryMu.Unlock()
}

func (proxy *Proxy) recordRequestWithUndo(
	record RequestRecord,
	declaration *adapter.Undo,
	input adapter.Request,
	responseBody []byte,
) {
	undoRecord, call := proxy.prepareUndo(declaration, input, responseBody)
	record.Undo = undoRecord
	proxy.summaryMu.Lock()
	index := len(proxy.summary.Requests)
	proxy.summary.Requests = append(proxy.summary.Requests, record)
	if call != nil {
		call.target = "request"
		call.index = index
		proxy.undoCalls = append(proxy.undoCalls, *call)
	} else if undoRecord != nil {
		proxy.undoCalls = append(proxy.undoCalls, undoCall{target: "request", index: index})
	}
	proxy.summaryMu.Unlock()
}

func (proxy *Proxy) prepareUndo(
	declaration *adapter.Undo,
	input adapter.Request,
	responseBody []byte,
) (*UndoRecord, *undoCall) {
	if declaration == nil {
		return nil, nil
	}
	record := &UndoRecord{
		Method: declaration.Method, URL: declaration.URL,
		Effect: undoEffect(declaration), StillExists: undoStillExists(declaration),
		State: "unavailable",
	}
	rendered, err := adapter.RenderUndo(declaration, input, responseBody)
	if err != nil {
		record.Error = err.Error()
		return record, nil
	}
	record.Method = rendered.Method
	record.URL = rendered.URL
	record.State = "available"
	headers := input.Header.Clone()
	removeHopByHopHeaders(headers)
	headers.Del("Content-Length")
	headers.Del("Idempotency-Key")
	for name, value := range rendered.Headers {
		headers.Set(name, value)
	}
	return record, &undoCall{
		method: rendered.Method, url: rendered.URL, header: headers,
		body: []byte(rendered.Body),
	}
}

func (proxy *Proxy) runUndos(ctx context.Context) []error {
	proxy.summaryMu.Lock()
	calls := append([]undoCall(nil), proxy.undoCalls...)
	proxy.undoCalls = nil
	proxy.summaryMu.Unlock()

	var undoErrors []error
	for _, call := range calls {
		if call.method == "" || call.url == "" {
			undoErrors = append(undoErrors, fmt.Errorf(
				"compensation is unavailable; %s", proxy.undoRemaining(call)))
			continue
		}
		proxy.updateUndo(call, func(record *UndoRecord) {
			record.State = "undoing"
			record.Error = ""
		})
		if err := proxy.persistStagedUpdate(); err != nil {
			proxy.updateUndo(call, func(record *UndoRecord) {
				record.State = "failed"
				record.Error = "record compensation before sending: " + err.Error()
			})
			undoErrors = append(undoErrors, fmt.Errorf(
				"did not attempt compensation %s; %s: %w",
				call.method+" "+call.url, proxy.undoRemaining(call), err,
			))
			continue
		}

		status, err := proxy.executeUndo(ctx, call)
		proxy.updateUndo(call, func(record *UndoRecord) {
			record.StatusCode = status
			if err != nil {
				record.State = "failed"
				record.Error = err.Error()
			} else {
				record.State = "succeeded"
			}
		})
		if persistErr := proxy.persistStagedUpdate(); persistErr != nil {
			err = errors.Join(err, fmt.Errorf("record compensation outcome: %w", persistErr))
		}
		if err != nil {
			undoErrors = append(undoErrors, fmt.Errorf(
				"compensation %s failed; %s: %w",
				call.method+" "+call.url, proxy.undoRemaining(call), err,
			))
		}
	}
	return undoErrors
}

func (proxy *Proxy) executeUndo(ctx context.Context, call undoCall) (int, error) {
	body := io.NopCloser(bytes.NewReader(call.body))
	request, err := http.NewRequestWithContext(ctx, call.method, call.url, body)
	if err != nil {
		return 0, fmt.Errorf("build compensation request: %w", err)
	}
	request.ContentLength = int64(len(call.body))
	request.GetBody = nil
	request.Header = call.header.Clone()
	response, err := proxy.transport.RoundTrip(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, maximumClassifiedBody+1))
	if readErr != nil {
		return response.StatusCode, fmt.Errorf("read compensation response: %w", readErr)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return response.StatusCode, fmt.Errorf(
			"compensation origin returned HTTP %d", response.StatusCode)
	}
	var envelope struct {
		OK *bool `json:"ok"`
	}
	if json.Unmarshal(responseBody, &envelope) == nil && envelope.OK != nil && !*envelope.OK {
		return response.StatusCode, errors.New(
			"compensation origin returned ok=false despite a successful HTTP status")
	}
	return response.StatusCode, nil
}

func (proxy *Proxy) updateUndo(call undoCall, change func(*UndoRecord)) {
	proxy.summaryMu.Lock()
	defer proxy.summaryMu.Unlock()
	var record *UndoRecord
	switch call.target {
	case "request":
		if call.index < len(proxy.summary.Requests) {
			record = proxy.summary.Requests[call.index].Undo
		}
	case "staged":
		if call.index < len(proxy.summary.Staged) {
			record = proxy.summary.Staged[call.index].Undo
		}
	}
	if record != nil {
		change(record)
	}
}

func (proxy *Proxy) undoRemaining(call undoCall) string {
	proxy.summaryMu.Lock()
	defer proxy.summaryMu.Unlock()
	var record *UndoRecord
	if call.target == "request" && call.index < len(proxy.summary.Requests) {
		record = proxy.summary.Requests[call.index].Undo
	}
	if call.target == "staged" && call.index < len(proxy.summary.Staged) {
		record = proxy.summary.Staged[call.index].Undo
	}
	if record == nil || record.StillExists == "" {
		return "the original external effect may still exist"
	}
	return record.StillExists
}

func undoEffect(declaration *adapter.Undo) string {
	if declaration.Effect != "" {
		return declaration.Effect
	}
	return strings.ToLower(declaration.Method) + " the resource using " + declaration.URL
}

func undoStillExists(declaration *adapter.Undo) string {
	if declaration.StillExists != "" {
		return declaration.StillExists
	}
	return "if compensation fails, the original external effect remains in the world"
}

func cloneUndoRecord(record *UndoRecord) *UndoRecord {
	if record == nil {
		return nil
	}
	clone := *record
	return &clone
}

func joinWarning(existing, warning string) string {
	if existing == "" {
		return warning
	}
	return existing + "; " + warning
}

type boundedCapture struct {
	data []byte
}

func (capture *boundedCapture) Write(data []byte) (int, error) {
	remaining := maximumClassifiedBody + 1 - len(capture.data)
	if remaining > 0 {
		if len(data) < remaining {
			remaining = len(data)
		}
		capture.data = append(capture.data, data[:remaining]...)
	}
	return len(data), nil
}

func (capture *boundedCapture) Bytes() []byte {
	return append([]byte(nil), capture.data...)
}

func (proxy *Proxy) recordUnintercepted(host, detail string) {
	proxy.summaryMu.Lock()
	proxy.summary.Unintercepted = append(proxy.summary.Unintercepted, UninterceptedItem{
		Host: host, Detail: detail, Time: time.Now().UTC(),
	})
	proxy.summaryMu.Unlock()
}
