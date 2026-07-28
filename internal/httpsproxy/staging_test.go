package httpsproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyj28/unring/internal/adapter"
)

func TestSlackPostMessageIsStagedAndCommitOrDiscardControlsSending(t *testing.T) {
	t.Run("commit", func(t *testing.T) {
		recorder := &recordingTransport{}
		proxy, client := startClassifiedProxy(t, recorder, nil)
		body := []byte(`{"channel":"C123","text":"hello from unring"}`)
		response := postThroughProxy(t, client,
			"https://slack.com/api/chat.postMessage", body)
		responseBody, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatalf("read synthesized Slack response: %v", err)
		}
		var decoded struct {
			OK     bool   `json:"ok"`
			TS     string `json:"ts"`
			Unring struct {
				Staged       bool `json:"staged"`
				RealResponse bool `json:"real_response"`
			} `json:"unring"`
		}
		if err := json.Unmarshal(responseBody, &decoded); err != nil {
			t.Fatalf("Slack client could not decode synthesized response: %v", err)
		}
		if response.StatusCode != http.StatusOK ||
			response.Header.Get("X-Unring-Staged") != "true" ||
			!decoded.OK || !decoded.Unring.Staged || decoded.Unring.RealResponse ||
			decoded.TS != "" {
			t.Fatalf("synthesized response can be mistaken for a real result: status=%d headers=%v body=%s",
				response.StatusCode, response.Header, responseBody)
		}
		if got := recorder.Count(); got != 0 {
			t.Fatalf("stageable Slack call reached origin before commit: %d requests", got)
		}
		summary := proxy.Summary()
		if len(summary.Staged) != 1 || summary.Staged[0].State != "pending" ||
			summary.Staged[0].Adapter != "slack" {
			t.Fatalf("pending Slack summary = %#v", summary)
		}

		sealProxy(t, proxy)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := proxy.Finalize(ctx, true); err != nil {
			t.Fatalf("Finalize(commit) error: %v", err)
		}
		requests := recorder.Requests()
		if len(requests) != 1 {
			t.Fatalf("commit sent %d requests, want 1", len(requests))
		}
		hash := sha256.Sum256(body)
		wantKey := "slack-message:" + hex.EncodeToString(hash[:])
		if got := requests[0].Header.Get("Idempotency-Key"); got != wantKey {
			t.Fatalf("replay idempotency key = %q, want %q", got, wantKey)
		}
		if got := string(requests[0].body); got != string(body) {
			t.Fatalf("replay body = %q, want %q", got, body)
		}
		if state := proxy.Summary().Staged[0].State; state != "sent" {
			t.Fatalf("final staged state = %q, want sent", state)
		}
	})

	t.Run("discard", func(t *testing.T) {
		recorder := &recordingTransport{}
		proxy, client := startClassifiedProxy(t, recorder, nil)
		response := postThroughProxy(t, client,
			"https://slack.com/api/chat.postMessage",
			[]byte(`{"channel":"C123","text":"discard me"}`))
		_ = response.Body.Close()
		sealProxy(t, proxy)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := proxy.Finalize(ctx, false); err != nil {
			t.Fatalf("Finalize(discard) error: %v", err)
		}
		if got := recorder.Count(); got != 0 {
			t.Fatalf("discard sent %d Slack requests, want 0", got)
		}
		if state := proxy.Summary().Staged[0].State; state != "discarded" {
			t.Fatalf("final staged state = %q, want discarded", state)
		}
	})
}

func TestGitHubIssueCreationNeedsApproval(t *testing.T) {
	for _, test := range []struct {
		name       string
		approved   bool
		wantStatus int
		wantSent   int
	}{
		{name: "declined", approved: false, wantStatus: http.StatusForbidden, wantSent: 0},
		{name: "approved", approved: true, wantStatus: http.StatusCreated, wantSent: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := &recordingTransport{status: http.StatusCreated}
			proxy, client := startClassifiedProxy(t, recorder,
				func(context.Context, ApprovalRequest) (bool, error) {
					return test.approved, nil
				})
			response := postThroughProxy(t, client,
				"https://api.github.com/repos/acme/widget/issues",
				[]byte(`{"title":"Issue from agent"}`))
			_ = response.Body.Close()
			if response.StatusCode != test.wantStatus {
				t.Fatalf("response status = %d, want %d", response.StatusCode, test.wantStatus)
			}
			if got := recorder.Count(); got != test.wantSent {
				t.Fatalf("origin received %d requests, want %d", got, test.wantSent)
			}
			summary := proxy.Summary()
			if len(summary.Approvals) != 1 ||
				summary.Approvals[0].Decision != test.name ||
				summary.Approvals[0].Adapter != "github" {
				t.Fatalf("GitHub approval summary = %#v", summary)
			}
		})
	}
}

func TestUnknownMutatingRequestDefaultsToNeedsApproval(t *testing.T) {
	recorder := &recordingTransport{}
	var requested ApprovalRequest
	proxy, client := startClassifiedProxy(t, recorder,
		func(_ context.Context, request ApprovalRequest) (bool, error) {
			requested = request
			return false, nil
		})
	response := postThroughProxy(t, client,
		"https://unknown-service.example/v9/do-something",
		[]byte(`{"dangerous":true}`))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden ||
		response.Header.Get("X-Unring-Approval") != "declined" {
		t.Fatalf("unknown request response = %d, headers %v", response.StatusCode, response.Header)
	}
	if recorder.Count() != 0 {
		t.Fatal("unknown mutating request was forwarded without approval")
	}
	if requested.Adapter != "" ||
		!strings.Contains(requested.Reason, "no adapter or safe HTTP heuristic") {
		t.Fatalf("unknown approval request = %#v", requested)
	}
	if summary := proxy.Summary(); len(summary.Approvals) != 1 ||
		summary.Approvals[0].Decision != "declined" {
		t.Fatalf("unknown request summary = %#v", summary)
	}
}

func TestStagedReplayIsNeverAutomaticallyRetried(t *testing.T) {
	var mu sync.Mutex
	received := 0
	origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		mu.Lock()
		received++
		call := received
		mu.Unlock()
		if call == 2 {
			connection, _, err := response.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack second replay: %v", err)
				return
			}
			_ = connection.Close()
			return
		}
		response.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(response, "ok")
	}))
	defer origin.Close()
	originURL, _ := url.Parse(origin.URL)
	transport := &http.Transport{MaxIdleConnsPerHost: 1}
	defer transport.CloseIdleConnections()
	proxy := &Proxy{
		transport: transport,
		summary: Summary{Sealed: true, Staged: []StagedRequest{
			{Method: http.MethodPost, URL: origin.URL + "/one", State: "pending"},
			{Method: http.MethodPost, URL: origin.URL + "/two", State: "pending"},
		}},
		staged: []stagedCall{
			{method: http.MethodPost, url: cloneURL(originURL), host: originURL.Host,
				header: make(http.Header), body: []byte("first"), key: "key-one"},
			{method: http.MethodPost, url: cloneURL(originURL), host: originURL.Host,
				header: make(http.Header), body: nil, key: "key-two"},
		},
	}
	proxy.staged[0].url.Path = "/one"
	proxy.staged[1].url.Path = "/two"

	err := proxy.Finalize(context.Background(), true)
	if err == nil {
		t.Fatal("replay whose response connection vanished unexpectedly succeeded")
	}
	mu.Lock()
	gotReceived := received
	mu.Unlock()
	if gotReceived != 2 {
		t.Fatalf("origin received %d POSTs, want exactly 2 (no transparent retry)", gotReceived)
	}
	summary := proxy.Summary()
	if summary.Staged[0].State != "sent" || summary.Staged[1].State != "unknown" {
		t.Fatalf("at-most-once replay states = %#v", summary.Staged)
	}
}

func TestSynthesizedMarkerCannotBeOverriddenDuringEmission(t *testing.T) {
	for iteration := 0; iteration < 1000; iteration++ {
		proxy := &Proxy{}
		requestURL, _ := url.Parse("https://slack.com/api/chat.postMessage")
		request := &http.Request{
			Method: http.MethodPost, URL: requestURL, Header: make(http.Header),
		}
		var wire bytes.Buffer
		_, err := proxy.stage(&wire, request, nil, adapter.Classification{
			Tier: adapter.TierStageable, Adapter: "adversarial", Rule: "marker",
			IdempotencyKey: "key",
			Response: &adapter.SyntheticResponse{
				Status: http.StatusOK,
				Headers: map[string]string{
					"X-Unring-Staged": "true",
					"x-unring-staged": "false",
				},
				Body: `{"ok":true}`,
			},
		})
		if err != nil {
			t.Fatalf("iteration %d stage() error: %v", iteration, err)
		}
		response, err := http.ReadResponse(bufio.NewReader(&wire), request)
		if err != nil {
			t.Fatalf("iteration %d decode synthesized response: %v", iteration, err)
		}
		_ = response.Body.Close()
		if got := response.Header.Get("X-Unring-Staged"); got != "true" {
			t.Fatalf("iteration %d emitted staged marker = %q, want true", iteration, got)
		}
	}
}

func TestAcceptedReplayWithTruncatedResponseBodyIsStillSent(t *testing.T) {
	proxy := proxyWithOneStagedCall(testRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       &unexpectedEOFBody{},
			Request:    request,
		}, nil
	}))
	if err := proxy.Finalize(context.Background(), true); err != nil {
		t.Fatalf("accepted replay Finalize() error: %v", err)
	}
	staged := proxy.Summary().Staged[0]
	if staged.State != "sent" || staged.ReplayStatusCode != http.StatusOK ||
		!strings.Contains(staged.Warning, "origin accepted") || staged.Error != "" {
		t.Fatalf("accepted truncated response outcome = %#v", staged)
	}
}

func TestNon2xxReplayOutcomeIsUnknownNotSendFailed(t *testing.T) {
	proxy := proxyWithOneStagedCall(testRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("redirect not followed")),
			Request:    request,
		}, nil
	}))
	if err := proxy.Finalize(context.Background(), true); err == nil {
		t.Fatal("non-2xx replay unexpectedly produced a confirmed outcome")
	}
	staged := proxy.Summary().Staged[0]
	if staged.State != "unknown" || staged.ReplayStatusCode != http.StatusFound ||
		!strings.Contains(staged.Error, "delivery outcome is unknown") {
		t.Fatalf("non-2xx replay outcome = %#v", staged)
	}
}

func TestReplayTransitionsArePublishedIncrementally(t *testing.T) {
	var snapshots []Summary
	proxy := &Proxy{
		transport: testRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader("ok")), Request: request,
			}, nil
		}),
		summary: Summary{Sealed: true, Staged: []StagedRequest{
			{Method: http.MethodPost, URL: "https://example.test/one", State: "pending"},
			{Method: http.MethodPost, URL: "https://example.test/two", State: "pending"},
		}},
		staged: []stagedCall{
			newTestStagedCall("https://example.test/one"),
			newTestStagedCall("https://example.test/two"),
		},
	}
	proxy.stagedUpdated = func(summary Summary) error {
		snapshots = append(snapshots, summary)
		return nil
	}
	if err := proxy.Finalize(context.Background(), true); err != nil {
		t.Fatalf("Finalize() error: %v", err)
	}
	want := [][]string{
		{"sending", "pending"},
		{"sent", "pending"},
		{"sent", "sending"},
		{"sent", "sent"},
	}
	if len(snapshots) < len(want) {
		t.Fatalf("published %d snapshots, want at least %d", len(snapshots), len(want))
	}
	for index, states := range want {
		got := []string{snapshots[index].Staged[0].State, snapshots[index].Staged[1].State}
		if got[0] != states[0] || got[1] != states[1] {
			t.Fatalf("snapshot %d states = %v, want %v", index, got, states)
		}
	}
}

type testRoundTripperFunc func(*http.Request) (*http.Response, error)

func (function testRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type unexpectedEOFBody struct {
	read bool
}

func (body *unexpectedEOFBody) Read(buffer []byte) (int, error) {
	if body.read {
		return 0, io.ErrUnexpectedEOF
	}
	body.read = true
	return copy(buffer, "partial"), nil
}

func (*unexpectedEOFBody) Close() error { return nil }

func proxyWithOneStagedCall(transport http.RoundTripper) *Proxy {
	return &Proxy{
		transport: transport,
		summary: Summary{Sealed: true, Staged: []StagedRequest{{
			Method: http.MethodPost, URL: "https://example.test/action", State: "pending",
		}}},
		staged: []stagedCall{newTestStagedCall("https://example.test/action")},
	}
}

func newTestStagedCall(rawURL string) stagedCall {
	parsed, _ := url.Parse(rawURL)
	return stagedCall{
		method: http.MethodPost, url: parsed, host: parsed.Host,
		header: make(http.Header), body: []byte(`{"value":true}`), key: "test-key",
	}
}

type recordedRequest struct {
	Method string
	URL    string
	Header http.Header
	body   []byte
}

type recordingTransport struct {
	mu       sync.Mutex
	requests []recordedRequest
	status   int
}

func (transport *recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	transport.mu.Lock()
	transport.requests = append(transport.requests, recordedRequest{
		Method: request.Method, URL: request.URL.String(),
		Header: request.Header.Clone(), body: body,
	})
	transport.mu.Unlock()
	status := transport.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"origin":true}`))),
		Request:    request,
	}, nil
}

func (transport *recordingTransport) Count() int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return len(transport.requests)
}

func (transport *recordingTransport) Requests() []recordedRequest {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	result := make([]recordedRequest, len(transport.requests))
	copy(result, transport.requests)
	return result
}

func startClassifiedProxy(
	t *testing.T,
	transport http.RoundTripper,
	approve func(context.Context, ApprovalRequest) (bool, error),
) (*Proxy, *http.Client) {
	t.Helper()
	sources, err := adapter.BuiltinSources()
	if err != nil {
		t.Fatalf("BuiltinSources() error: %v", err)
	}
	adapters, err := adapter.Load(sources...)
	if err != nil {
		t.Fatalf("Load(builtins) error: %v", err)
	}
	authority, err := EnsureAuthority(t.TempDir())
	if err != nil {
		t.Fatalf("EnsureAuthority() error: %v", err)
	}
	proxy, err := Start(authority, Options{
		Transport: transport, Adapters: adapters, Approve: approve,
	})
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	t.Cleanup(func() {
		_ = proxy.Close()
	})
	proxyURL, _ := url.Parse("http://" + proxy.Address())
	roots := x509.NewCertPool()
	roots.AddCert(authority.Certificate)
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			RootCAs: roots, MinVersion: tls.VersionTLS12,
		},
	}}
	return proxy, client
}

func postThroughProxy(t *testing.T, client *http.Client, endpoint string, body []byte) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build POST: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("POST through proxy: %v", err)
	}
	return response
}
