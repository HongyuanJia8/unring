// Package httpsproxy implements unring's loopback HTTPS interception proxy.
package httpsproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hyj28/unring/internal/adapter"
)

type tlsCertificate = tls.Certificate

// RequestRecord is one HTTPS request that was actually sent to an origin.
type RequestRecord struct {
	Method     string       `json:"method"`
	URL        string       `json:"url"`
	Tier       adapter.Tier `json:"tier,omitempty"`
	Adapter    string       `json:"adapter,omitempty"`
	Rule       string       `json:"rule,omitempty"`
	StatusCode int          `json:"status_code,omitempty"`
	Error      string       `json:"error,omitempty"`
	StartedAt  time.Time    `json:"started_at"`
	EndedAt    time.Time    `json:"ended_at"`
}

// StagedRequest is a call withheld from the origin until the final decision.
type StagedRequest struct {
	Method           string    `json:"method"`
	URL              string    `json:"url"`
	Adapter          string    `json:"adapter"`
	Rule             string    `json:"rule"`
	IdempotencyKey   string    `json:"idempotency_key"`
	SyntheticStatus  int       `json:"synthetic_status"`
	State            string    `json:"state"`
	ReplayStatusCode int       `json:"replay_status_code,omitempty"`
	Error            string    `json:"error,omitempty"`
	Warning          string    `json:"warning,omitempty"`
	StagedAt         time.Time `json:"staged_at"`
	Body             string    `json:"-"`
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

// HasReviewableActivity reports whether any HTTPS coverage or forwarding was observed.
func (summary Summary) HasReviewableActivity() bool {
	return !summary.Sealed || len(summary.Requests) > 0 || len(summary.Staged) > 0 ||
		len(summary.Approvals) > 0 || len(summary.Unintercepted) > 0
}

// Options configures upstream forwarding and explicit CONNECT passthrough.
type Options struct {
	Transport       http.RoundTripper
	PassthroughHost func(host string) bool
	Adapters        *adapter.Set
	Approve         func(context.Context, ApprovalRequest) (bool, error)
	StagedUpdated   func(Summary) error
}

// Proxy intercepts HTTPS CONNECT connections on loopback.
type Proxy struct {
	authority     *Authority
	listener      net.Listener
	server        *http.Server
	transport     http.RoundTripper
	passthrough   func(string) bool
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
		transport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     false,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		}
	}
	proxy := &Proxy{
		authority: authority, listener: listener, transport: transport,
		passthrough: options.PassthroughHost, adapters: options.Adapters,
		approve: options.Approve, stagedUpdated: options.StagedUpdated,
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

// Address returns the loopback address of the proxy.
func (proxy *Proxy) Address() string {
	return proxy.listener.Addr().String()
}

// Summary returns a detached activity snapshot.
func (proxy *Proxy) Summary() Summary {
	proxy.summaryMu.Lock()
	defer proxy.summaryMu.Unlock()
	return Summary{
		Connections:   proxy.summary.Connections,
		Requests:      append([]RequestRecord(nil), proxy.summary.Requests...),
		Staged:        append([]StagedRequest(nil), proxy.summary.Staged...),
		Approvals:     append([]ApprovalRecord(nil), proxy.summary.Approvals...),
		Unintercepted: append([]UninterceptedItem(nil), proxy.summary.Unintercepted...),
		Sealed:        proxy.summary.Sealed,
		Finalized:     proxy.summary.Finalized,
	}
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
			proxy.summary.Finalized = true
			proxy.summaryMu.Unlock()
			if err := proxy.persistStagedUpdate(); err != nil {
				proxy.finalizeErr = fmt.Errorf("record discarded staged requests: %w", err)
			}
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
			}
			proxy.summaryMu.Unlock()
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
	status  int
	state   string
	warning string
	err     error
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
	_, readErr := io.Copy(io.Discard, response.Body)
	if response.StatusCode >= 200 && response.StatusCode <= 299 {
		result := replayResult{status: response.StatusCode, state: "sent"}
		if readErr != nil {
			result.warning = fmt.Sprintf(
				"origin accepted the request with HTTP %d, but reading its response body failed: %v",
				response.StatusCode, readErr,
			)
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
	const maximumClassifiedBody = 16 << 20
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

	classification, matched, err := proxy.classify(request, body)
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
		return proxy.forwardActual(client, clientReader, request, classification)
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
	}
	proxy.summaryMu.Lock()
	proxy.staged = append(proxy.staged, call)
	proxy.summary.Staged = append(proxy.summary.Staged, StagedRequest{
		Method: request.Method, URL: request.URL.String(),
		Adapter: classification.Adapter, Rule: classification.Rule,
		IdempotencyKey:  classification.IdempotencyKey,
		SyntheticStatus: classification.Response.Status,
		State:           "pending", StagedAt: started, Body: string(body),
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
	return proxy.forwardActual(client, clientReader, request, classification)
}

func (proxy *Proxy) forwardActual(
	client io.ReadWriteCloser,
	clientReader io.Reader,
	request *http.Request,
	classification adapter.Classification,
) (bool, error) {
	started := time.Now().UTC()
	record := RequestRecord{
		Method: request.Method, URL: request.URL.String(),
		Tier: classification.Tier, Adapter: classification.Adapter,
		Rule: classification.Rule, StartedAt: started,
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
		record.Error = err.Error()
		record.EndedAt = time.Now().UTC()
		proxy.recordRequest(record)
		_, _ = io.WriteString(client,
			"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return true, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusSwitchingProtocols {
		return proxy.forwardUpgrade(
			client, clientReader, request, response, request.URL.Host, requestedUpgrade,
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
	if err := response.Write(client); err != nil {
		record.Error = err.Error()
		record.EndedAt = time.Now().UTC()
		proxy.recordRequest(record)
		return true, err
	}
	record.EndedAt = time.Now().UTC()
	proxy.recordRequest(record)
	return closeClient, nil
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

	proxy.recordUnintercepted(hostport, fmt.Sprintf(
		"HTTP protocol upgrade to %q was tunneled without payload interception",
		responseUpgrade,
	))
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

func (proxy *Proxy) recordUnintercepted(host, detail string) {
	proxy.summaryMu.Lock()
	proxy.summary.Unintercepted = append(proxy.summary.Unintercepted, UninterceptedItem{
		Host: host, Detail: detail, Time: time.Now().UTC(),
	})
	proxy.summaryMu.Unlock()
}
