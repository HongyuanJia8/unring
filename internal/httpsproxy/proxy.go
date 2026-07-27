// Package httpsproxy implements unring's loopback HTTPS interception proxy.
package httpsproxy

import (
	"bufio"
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
)

type tlsCertificate = tls.Certificate

// RequestRecord is one HTTPS request that was intercepted and forwarded.
type RequestRecord struct {
	Method     string    `json:"method"`
	URL        string    `json:"url"`
	StatusCode int       `json:"status_code,omitempty"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at"`
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
	Unintercepted []UninterceptedItem `json:"unintercepted"`
	Sealed        bool                `json:"sealed"`
}

// HasReviewableActivity reports whether any HTTPS coverage or forwarding was observed.
func (summary Summary) HasReviewableActivity() bool {
	return !summary.Sealed || len(summary.Requests) > 0 || len(summary.Unintercepted) > 0
}

// Options configures upstream forwarding and explicit CONNECT passthrough.
type Options struct {
	Transport       http.RoundTripper
	PassthroughHost func(host string) bool
}

// Proxy intercepts HTTPS CONNECT connections on loopback.
type Proxy struct {
	authority   *Authority
	listener    net.Listener
	server      *http.Server
	transport   http.RoundTripper
	passthrough func(string) bool
	runCtx      context.Context
	cancel      context.CancelFunc

	connectionsMu sync.Mutex
	connections   map[net.Conn]struct{}
	connectionWG  sync.WaitGroup
	handlerWG     sync.WaitGroup
	sealing       bool

	summaryMu sync.Mutex
	summary   Summary

	closeOnce sync.Once
	closeErr  error
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
		passthrough: options.PassthroughHost, connections: make(map[net.Conn]struct{}),
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
		Unintercepted: append([]UninterceptedItem(nil), proxy.summary.Unintercepted...),
		Sealed:        proxy.summary.Sealed,
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

// Close seals the proxy with a bounded timeout.
func (proxy *Proxy) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return proxy.Seal(ctx)
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
	started := time.Now().UTC()
	record := RequestRecord{Method: request.Method, StartedAt: started}
	request.RequestURI = ""
	request.URL.Scheme = "https"
	request.URL.Host = hostport
	request = request.WithContext(proxy.runCtx)
	if request.Host == "" {
		request.Host = hostport
	}
	record.URL = request.URL.String()
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
			client, clientReader, request, response, hostport, requestedUpgrade,
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
