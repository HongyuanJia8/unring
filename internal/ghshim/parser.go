package ghshim

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

type intent struct {
	Method string
	URL    *url.URL
	Body   []byte
	Kind   string
	Detail string
	Read   bool
}

type flagSpec struct {
	takesValue bool
}

var globalFlags = map[string]flagSpec{
	"--hostname": {takesValue: true},
	"--repo":     {takesValue: true},
	"-R":         {takesValue: true},
}

var issueListFlags = map[string]flagSpec{
	"--app":       {takesValue: true},
	"--assignee":  {takesValue: true},
	"--author":    {takesValue: true},
	"--closed":    {},
	"--json":      {takesValue: true},
	"--jq":        {takesValue: true},
	"--label":     {takesValue: true},
	"--limit":     {takesValue: true},
	"--mention":   {takesValue: true},
	"--milestone": {takesValue: true},
	"--search":    {takesValue: true},
	"--state":     {takesValue: true},
	"--template":  {takesValue: true},
	"--web":       {},
	"--help":      {},
	"--hostname":  {takesValue: true},
	"--repo":      {takesValue: true},
	"-R":          {takesValue: true},
}

var issueCreateFlags = map[string]flagSpec{
	"--assignee":  {takesValue: true},
	"-a":          {takesValue: true},
	"--body":      {takesValue: true},
	"-b":          {takesValue: true},
	"--body-file": {takesValue: true},
	"-F":          {takesValue: true},
	"--editor":    {},
	"--label":     {takesValue: true},
	"-l":          {takesValue: true},
	"--milestone": {takesValue: true},
	"-m":          {takesValue: true},
	"--project":   {takesValue: true},
	"-p":          {takesValue: true},
	"--recover":   {takesValue: true},
	"--template":  {takesValue: true},
	"-T":          {takesValue: true},
	"--title":     {takesValue: true},
	"-t":          {takesValue: true},
	"--web":       {},
	"--help":      {},
}

func parse(args []string, repository string) (intent, error) {
	position := 0
	var err error
	repository, position, err = consumeFlags(args, position, globalFlags, repository)
	if err != nil {
		return intent{}, err
	}
	if position >= len(args) {
		return intent{}, fmt.Errorf("no gh subcommand was provided")
	}
	if args[position] != "issue" {
		return intent{}, fmt.Errorf("unrecognized gh subcommand %q", args[position])
	}
	position++
	if position >= len(args) {
		return intent{}, fmt.Errorf("no gh issue subcommand was provided")
	}
	subcommand := args[position]
	position++
	switch subcommand {
	case "list":
		_, position, err = consumeFlags(args, position, issueListFlags, repository)
		if err != nil {
			return intent{}, err
		}
		if position != len(args) {
			return intent{}, fmt.Errorf("unexpected argument %q for gh issue list", args[position])
		}
		endpoint := githubIssueEndpoint(repository)
		return intent{
			Method: "GET", URL: endpoint, Kind: "list GitHub issues",
			Detail: "gh issue list", Read: true,
		}, nil
	case "create":
		values := make(map[string][]string)
		repository, position, err = consumeCreateFlags(args, position, repository, values)
		if err != nil {
			return intent{}, err
		}
		if position != len(args) {
			return intent{}, fmt.Errorf("unexpected argument %q for gh issue create", args[position])
		}
		body := map[string]any{}
		if value := lastValue(values, "--title", "-t"); value != "" {
			body["title"] = value
		}
		if value := lastValue(values, "--body", "-b"); value != "" {
			body["body"] = value
		}
		if labels := append(values["--label"], values["-l"]...); len(labels) > 0 {
			body["labels"] = labels
		}
		encoded, _ := json.Marshal(body)
		return intent{
			Method: "POST", URL: githubIssueEndpoint(repository), Body: encoded,
			Kind: "create GitHub issue", Detail: describeIssueCreate(body),
		}, nil
	default:
		return intent{}, fmt.Errorf("unrecognized gh issue subcommand %q", subcommand)
	}
}

func consumeFlags(
	args []string,
	position int,
	specs map[string]flagSpec,
	repository string,
) (string, int, error) {
	for position < len(args) && strings.HasPrefix(args[position], "-") {
		name, inline, hasInline := strings.Cut(args[position], "=")
		spec, known := specs[name]
		if !known {
			return repository, position, fmt.Errorf("unrecognized flag %q", name)
		}
		position++
		if spec.takesValue {
			value := inline
			if !hasInline {
				if position >= len(args) {
					return repository, position, fmt.Errorf("flag %s requires a value", name)
				}
				value = args[position]
				position++
			}
			if name == "--repo" || name == "-R" {
				repository = value
			}
		} else if hasInline {
			return repository, position, fmt.Errorf("flag %s does not take a value", name)
		}
	}
	return repository, position, nil
}

func consumeCreateFlags(
	args []string,
	position int,
	repository string,
	values map[string][]string,
) (string, int, error) {
	for position < len(args) && strings.HasPrefix(args[position], "-") {
		name, inline, hasInline := strings.Cut(args[position], "=")
		spec, known := issueCreateFlags[name]
		if !known {
			if global, ok := globalFlags[name]; ok {
				spec = global
				known = true
			}
		}
		if !known {
			return repository, position, fmt.Errorf("unrecognized flag %q", name)
		}
		position++
		value := ""
		if spec.takesValue {
			value = inline
			if !hasInline {
				if position >= len(args) {
					return repository, position, fmt.Errorf("flag %s requires a value", name)
				}
				value = args[position]
				position++
			}
			values[name] = append(values[name], value)
			if name == "--repo" || name == "-R" {
				repository = value
			}
		} else if hasInline {
			return repository, position, fmt.Errorf("flag %s does not take a value", name)
		}
	}
	return repository, position, nil
}

func githubIssueEndpoint(repository string) *url.URL {
	repository = strings.Trim(repository, "/")
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		repository = "_current_/_current_"
	}
	endpoint, _ := url.Parse("https://api.github.com/repos/" + repository + "/issues")
	return endpoint
}

func lastValue(values map[string][]string, names ...string) string {
	for _, name := range names {
		items := values[name]
		if len(items) > 0 {
			return items[len(items)-1]
		}
	}
	return ""
}

func describeIssueCreate(body map[string]any) string {
	if title, ok := body["title"].(string); ok && title != "" {
		return fmt.Sprintf("create GitHub issue with title %q", title)
	}
	return "create GitHub issue (title may be collected interactively by gh)"
}
