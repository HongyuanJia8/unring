package ghshim

import (
	"net/http"
	"strings"
	"testing"
)

func TestEnumeratedReadOnlyInvocations(t *testing.T) {
	tests := [][]string{
		{"--version"},
		{"--help"},
		{"-h"},
		{"help", "pr", "view"},
		{"auth", "status"},
		{"auth", "status", "--hostname", "github.example"},
		{"status"},
		{"browse", "--repo", "acme/widget"},
		{"issue", "list", "--limit", "10"},
		{"issue", "view", "123", "--comments"},
		{"issue", "status"},
		{"pr", "list", "--state", "open"},
		{"pr", "view", "42", "--json", "title,state"},
		{"pr", "status"},
		{"repo", "list", "acme", "--limit", "10"},
		{"repo", "view", "acme/widget"},
		{"release", "list"},
		{"release", "view", "v1.0.0"},
		{"run", "list", "--limit", "5"},
		{"run", "view", "1234"},
		{"workflow", "list"},
		{"workflow", "view", "ci.yml"},
		{"project", "list", "--owner", "acme"},
		{"project", "view", "1", "--owner", "acme"},
		{"gist", "list"},
		{"search", "issues", "bug", "--repo", "acme/widget"},
		{"search", "prs", "fix", "--state", "open"},
	}
	for _, arguments := range tests {
		parsed, err := parse(arguments, "")
		if err != nil {
			t.Errorf("parse(%q) error: %v", arguments, err)
			continue
		}
		if !parsed.Read || parsed.Method != http.MethodGet {
			t.Errorf("parse(%q) = %#v, want read-only GET", arguments, parsed)
		}
	}
}

func TestUnknownReadSubcommandOrFlagStillNeedsApproval(t *testing.T) {
	for _, arguments := range [][]string{
		{"pr", "checks", "42"},
		{"repo", "view", "--future-flag"},
		{"search", "mysteries", "query"},
	} {
		parsed, err := parse(arguments, "")
		if err == nil || parsed.Read {
			t.Errorf("parse(%q) = %#v, %v; want ambiguity error", arguments, parsed, err)
		}
	}
}

func TestAPIGetVersusFieldsAndMethods(t *testing.T) {
	for _, arguments := range [][]string{
		{"api", "repos/acme/widget"},
		{"api", "repos/acme/widget", "--method", "GET"},
		{"api", "repos/acme/widget", "-XGET"},
		{"api", "repos/acme/widget", "--header", "Accept: application/json"},
	} {
		parsed, err := parse(arguments, "")
		if err != nil || !parsed.Read || parsed.Method != http.MethodGet {
			t.Errorf("parse(%q) = %#v, %v; want read-only GET", arguments, parsed, err)
		}
	}

	for _, test := range []struct {
		arguments []string
		method    string
		reason    string
	}{
		{
			arguments: []string{"api", "repos/acme/widget/hooks", "-f", "name=deploy"},
			method:    http.MethodPost, reason: "-f supplies request data",
		},
		{
			arguments: []string{"api", "repos/acme/widget", "--method", "PATCH"},
			method:    http.MethodPatch, reason: "gh api uses PATCH",
		},
		{
			arguments: []string{"api", "graphql"},
			method:    http.MethodPost, reason: "gh api uses POST",
		},
		{
			arguments: []string{"api", "repos/acme/widget", "--method", "GET", "-F", "page=2"},
			method:    http.MethodGet, reason: "-F supplies request data",
		},
	} {
		parsed, err := parse(test.arguments, "")
		if err != nil {
			t.Errorf("parse(%q) error: %v", test.arguments, err)
			continue
		}
		if parsed.Read || parsed.Method != test.method ||
			!strings.Contains(parsed.Reason, test.reason) {
			t.Errorf("parse(%q) = %#v, want mutating/ambiguous %s",
				test.arguments, parsed, test.method)
		}
	}
}

func TestUnparseableAPIRequiresApproval(t *testing.T) {
	for _, arguments := range [][]string{
		{"api"},
		{"api", "repos/acme/widget", "--unknown"},
		{"api", "one", "two"},
		{"api", "repos/acme/widget", "--method"},
		{"api", "repos/acme/widget", "-f", "missing-equals"},
	} {
		parsed, err := parse(arguments, "")
		if err == nil || parsed.Read {
			t.Errorf("parse(%q) = %#v, %v; want ambiguity error", arguments, parsed, err)
		}
	}
}
