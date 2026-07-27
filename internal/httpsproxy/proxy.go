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
			Proxy:                 nil,
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
	if request.Method != http.MethodConnect {
		http.Error(response, "unring HTTPS proxy accepts CONNECT tunnels only", http.StatusBadRequest)
		proxy.recordUnintercepted(request.Host,
			"non-CONNECT traffic reached the HTTPS-only proxy and was not intercepted")
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

func (proxy *Proxy) track(connection net.Conn) (func(), bool) {
	proxy.connectionsMu.Lock()
	if proxy.sealing {
		proxy.connectionsMu.Unlock()
		_ = connection.Close()
		return func() {}, false
	}
	proxy.connections[connection] = struct{}{}
	proxy.connectionsMu.Unlock()
	proxy.connectionWG.Add(1)
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
	var dialer net.Dialer
	upstream, err := dialer.DialContext(dialContext, "tcp", hostport)
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
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		copyDone <- struct{}{}
	}()
	_, _ = io.Copy(client, upstream)
	<-copyDone
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
		closeConnection := request.Close
		if err := proxy.forward(tlsClient, request, hostport); err != nil {
			return
		}
		if closeConnection {
			return
		}
	}
}

func (proxy *Proxy) forward(client io.Writer, request *http.Request, hostport string) error {
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
	removeHopByHopHeaders(request.Header)
	response, err := proxy.transport.RoundTrip(request)
	if err != nil {
		record.Error = err.Error()
		record.EndedAt = time.Now().UTC()
		proxy.recordRequest(record)
		_, _ = io.WriteString(client,
			"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return err
	}
	defer response.Body.Close()
	record.StatusCode = response.StatusCode
	removeHopByHopHeaders(response.Header)
	if err := response.Write(client); err != nil {
		record.Error = err.Error()
		record.EndedAt = time.Now().UTC()
		proxy.recordRequest(record)
		return err
	}
	record.EndedAt = time.Now().UTC()
	proxy.recordRequest(record)
	return nil
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
