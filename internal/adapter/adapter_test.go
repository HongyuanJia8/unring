package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinsAndUserAdapterUseSameLoaderPath(t *testing.T) {
	builtins, err := BuiltinSources()
	if err != nil {
		t.Fatalf("BuiltinSources() error: %v", err)
	}
	user := Source{
		Name: "community-example.yaml",
		Data: []byte(`
version: 1
name: community-example
rules:
  - name: conditional-notification
    match:
      hosts: [api.never-heard-of.example]
      methods: [POST]
      path: /v1/actions/*
    when: 'body.kind == "notification" && request.headers["x-mode"] == "async"'
    tier: stageable
    idempotency_key: '"community:" + request.body_sha256'
    response:
      status: 202
      headers:
        Content-Type: application/json
        X-Unring-Staged: "true"
      body: '{"accepted":true,"unring_staged":true}'
    undo:
      method: DELETE
      url: https://api.never-heard-of.example/v1/actions/${response.id}
`),
	}
	set, err := Load(append(builtins, user)...)
	if err != nil {
		t.Fatalf("Load(builtins + user) error: %v", err)
	}
	if got := len(set.Adapters()); got != 3 {
		t.Fatalf("loaded adapters = %d, want 3", got)
	}

	slackBody := []byte(`{"channel":"C123","text":"hello"}`)
	slackURL, _ := url.Parse("https://slack.com/api/chat.postMessage")
	slack, matched, err := set.Classify(Request{
		Method: http.MethodPost, URL: slackURL,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   slackBody,
	})
	if err != nil || !matched {
		t.Fatalf("classify Slack = %#v, %v, matched=%v", slack, err, matched)
	}
	hash := sha256.Sum256(slackBody)
	if slack.Tier != TierStageable || slack.Adapter != "slack" ||
		slack.IdempotencyKey != "slack-message:"+hex.EncodeToString(hash[:]) {
		t.Fatalf("Slack classification = %#v", slack)
	}

	githubURL, _ := url.Parse("https://api.github.com/repos/acme/widget/issues")
	github, matched, err := set.Classify(Request{
		Method: http.MethodPost, URL: githubURL,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   []byte(`{"title":"broken"}`),
	})
	if err != nil || !matched || github.Tier != TierNeedsApproval ||
		github.Adapter != "github" {
		t.Fatalf("GitHub classification = %#v, %v, matched=%v", github, err, matched)
	}

	communityURL, _ := url.Parse("https://api.never-heard-of.example/v1/actions/send")
	community, matched, err := set.Classify(Request{
		Method: http.MethodPost, URL: communityURL,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Mode":       []string{"async"},
		},
		Body: []byte(`{"kind":"notification"}`),
	})
	if err != nil || !matched || community.Tier != TierStageable ||
		community.Adapter != "community-example" {
		t.Fatalf("community classification = %#v, %v, matched=%v", community, err, matched)
	}

	_, matched, err = set.Classify(Request{
		Method: http.MethodPost, URL: communityURL,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Mode":       []string{"sync"},
		},
		Body: []byte(`{"kind":"notification"}`),
	})
	if err != nil || matched {
		t.Fatalf("false CEL condition matched=%v, error=%v", matched, err)
	}
}

func TestMalformedAndUnreadableAdaptersFailLoudly(t *testing.T) {
	t.Run("malformed", func(t *testing.T) {
		filename := filepath.Join(t.TempDir(), "broken-adapter.yaml")
		if err := os.WriteFile(filename, []byte(`
version: 1
name: broken
rules:
  - name: unsafe
    match:
      hosts: [example.com]
      methods: [POST]
      path: /events
    tier: stageable
    idempotency_key: 'request.missing('
    response:
      status: 200
      headers:
        X-Unring-Staged: "true"
`), 0o600); err != nil {
			t.Fatalf("write malformed adapter: %v", err)
		}
		sources, err := ReadFiles([]string{filename})
		if err != nil {
			t.Fatalf("ReadFiles() error: %v", err)
		}
		_, err = Load(sources...)
		if err == nil || !strings.Contains(err.Error(), filename) ||
			!strings.Contains(err.Error(), "compile idempotency_key") {
			t.Fatalf("malformed adapter error = %v", err)
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		filename := filepath.Join(t.TempDir(), "missing.yaml")
		_, err := ReadFiles([]string{filename})
		if err == nil || !strings.Contains(err.Error(), filename) {
			t.Fatalf("unreadable adapter error = %v", err)
		}
	})
}

func TestUnknownYAMLFieldFails(t *testing.T) {
	_, err := Load(Source{Name: "typo.yaml", Data: []byte(`
version: 1
name: typo
rulez: []
`)})
	if err == nil || !strings.Contains(err.Error(), "typo.yaml") ||
		!strings.Contains(err.Error(), "field rulez not found") {
		t.Fatalf("unknown field error = %v", err)
	}
}

func TestContradictoryStagedMarkerIsAlwaysRejected(t *testing.T) {
	data := []byte(`
version: 1
name: contradictory-marker
rules:
  - name: post
    match:
      hosts: [slack.com]
      methods: [POST]
      path: /api/chat.postMessage
    tier: stageable
    idempotency_key: 'request.body_sha256'
    response:
      status: 200
      headers:
        Content-Type: application/json
        X-Unring-Staged: "true"
        x-unring-staged: "false"
      body: '{"ok":true}'
`)
	for iteration := 0; iteration < 1000; iteration++ {
		_, err := Load(Source{Name: "contradictory-marker.yaml", Data: data})
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "x-unring-staged") {
			t.Fatalf("iteration %d contradictory marker error = %v", iteration, err)
		}
	}
}

func TestAdapterCannotSelectAlreadyIrreversibleTier(t *testing.T) {
	_, err := Load(Source{Name: "unprompted-charge.yaml", Data: []byte(`
version: 1
name: unprompted-charge
rules:
  - name: charge
    match:
      hosts: [api.acme.example]
      methods: [POST]
      path: /v1/charges
    tier: already-irreversible
`)})
	if err == nil || !strings.Contains(err.Error(), "reserved") ||
		!strings.Contains(err.Error(), "cannot be selected by an adapter") {
		t.Fatalf("already-irreversible adapter error = %v", err)
	}
}
