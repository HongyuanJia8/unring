package ghshim

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type intent struct {
	Method string
	URL    *url.URL
	Body   []byte
	Kind   string
	Detail string
	Reason string
	Read   bool
}

type flagSpec struct {
	takesValue bool
}

type readCommandSpec struct {
	flags          map[string]flagSpec
	minPositionals int
	maxPositionals int
}

var globalFlags = flagSet(
	[]string{"--hostname", "--repo", "-R"},
	nil,
)

var commonReadFlags = flagSet(
	[]string{"--hostname", "--repo", "-R", "--json", "--jq", "--template"},
	[]string{"--help", "-h", "--web"},
)

var readCommands = map[string]readCommandSpec{
	"auth status": {
		flags: mergeFlagSets(commonReadFlags, flagSet(
			nil,
			[]string{"--active", "--show-token"},
		)),
	},
	"status": {
		flags: mergeFlagSets(commonReadFlags, flagSet(
			[]string{"--exclude", "--org"},
			nil,
		)),
	},
	"browse": {
		flags: mergeFlagSets(commonReadFlags, flagSet(
			[]string{"--branch", "--commit"},
			[]string{"--no-browser", "--projects", "--releases", "--settings", "--wiki"},
		)),
		maxPositionals: 1,
	},
	"issue list": {
		flags: mergeFlagSets(commonReadFlags, flagSet(
			[]string{
				"--app", "--assignee", "--author", "--label", "--limit", "--mention",
				"--milestone", "--search", "--state",
			},
			[]string{"--closed"},
		)),
	},
	"issue view": {
		flags:          mergeFlagSets(commonReadFlags, flagSet(nil, []string{"--comments"})),
		maxPositionals: 1,
	},
	"issue status": {
		flags: commonReadFlags,
	},
	"pr list": {
		flags: mergeFlagSets(commonReadFlags, flagSet(
			[]string{
				"--app", "--assignee", "--author", "--base", "--head", "--label",
				"--limit", "--search", "--state",
			},
			[]string{"--draft"},
		)),
	},
	"pr view": {
		flags:          mergeFlagSets(commonReadFlags, flagSet(nil, []string{"--comments"})),
		maxPositionals: 1,
	},
	"pr status": {
		flags: mergeFlagSets(commonReadFlags, flagSet(nil, []string{"--conflict-status"})),
	},
	"repo list": {
		flags: mergeFlagSets(commonReadFlags, flagSet(
			[]string{"--language", "--limit", "--topic", "--visibility"},
			[]string{"--archived", "--fork", "--no-archived", "--source"},
		)),
		maxPositionals: 1,
	},
	"repo view": {
		flags: mergeFlagSets(commonReadFlags, flagSet(
			[]string{"--branch", "-b"},
			nil,
		)),
		maxPositionals: 1,
	},
	"release list": {
		flags: mergeFlagSets(commonReadFlags, flagSet(
			[]string{"--limit", "--order"},
			[]string{"--exclude-drafts", "--exclude-pre-releases"},
		)),
	},
	"release view": {
		flags:          commonReadFlags,
		maxPositionals: 1,
	},
	"run list": {
		flags: mergeFlagSets(commonReadFlags, flagSet(
			[]string{
				"--branch", "--commit", "--created", "--event", "--limit", "--status",
				"--user", "--workflow", "-w",
			},
			[]string{"--all"},
		)),
	},
	"run view": {
		flags: mergeFlagSets(commonReadFlags, flagSet(
			[]string{"--attempt", "--job"},
			[]string{"--exit-status", "--log", "--log-failed", "--verbose"},
		)),
		maxPositionals: 1,
	},
	"workflow list": {
		flags: mergeFlagSets(commonReadFlags, flagSet(
			[]string{"--limit"},
			[]string{"--all"},
		)),
	},
	"workflow view": {
		flags: mergeFlagSets(commonReadFlags, flagSet(
			[]string{"--ref"},
			[]string{"--yaml"},
		)),
		maxPositionals: 1,
	},
	"project list": {
		flags: mergeFlagSets(commonReadFlags, flagSet(
			[]string{"--limit", "--owner"},
			[]string{"--closed"},
		)),
	},
	"project view": {
		flags: mergeFlagSets(commonReadFlags, flagSet(
			[]string{"--format", "--owner"},
			nil,
		)),
		minPositionals: 1,
		maxPositionals: 1,
	},
	"gist list": {
		flags: mergeFlagSets(commonReadFlags, flagSet(
			[]string{"--limit"},
			[]string{"--public", "--secret"},
		)),
	},
	"search code": {
		flags:          searchFlags("--extension", "--filename", "--language", "--limit", "--match", "--owner", "--repo", "--size"),
		minPositionals: 1,
	},
	"search commits": {
		flags:          searchFlags("--author", "--author-date", "--committer", "--committer-date", "--hash", "--limit", "--merge", "--order", "--owner", "--parent", "--repo", "--sort", "--tree", "--visibility"),
		minPositionals: 1,
	},
	"search issues": {
		flags:          searchFlags("--app", "--archived", "--assignee", "--author", "--closed", "--commenter", "--comments", "--created", "--include-prs", "--interactions", "--involves", "--label", "--language", "--limit", "--locked", "--match", "--mentions", "--milestone", "--no-assignee", "--no-label", "--no-milestone", "--no-project", "--order", "--owner", "--project", "--reactions", "--repo", "--sort", "--state", "--team-mentions", "--updated", "--visibility"),
		minPositionals: 1,
	},
	"search prs": {
		flags:          searchFlags("--app", "--archived", "--assignee", "--author", "--base", "--checks", "--closed", "--commenter", "--comments", "--created", "--draft", "--head", "--interactions", "--involves", "--label", "--language", "--limit", "--locked", "--match", "--mentions", "--merged", "--merged-at", "--milestone", "--no-assignee", "--no-label", "--no-milestone", "--no-project", "--order", "--owner", "--project", "--reactions", "--repo", "--review", "--review-requested", "--reviewed-by", "--sort", "--state", "--team-mentions", "--updated", "--visibility"),
		minPositionals: 1,
	},
	"search repos": {
		flags:          searchFlags("--archived", "--created", "--followers", "--forks", "--good-first-issues", "--help-wanted-issues", "--include-forks", "--language", "--license", "--limit", "--match", "--number-topics", "--order", "--owner", "--size", "--sort", "--stars", "--template", "--topic", "--updated", "--visibility", "--watchers"),
		minPositionals: 1,
	},
}

var issueCreateFlags = mergeFlagSets(globalFlags, flagSet(
	[]string{
		"--assignee", "-a", "--body", "-b", "--body-file", "-F", "--label", "-l",
		"--milestone", "-m", "--project", "-p", "--recover", "--template", "-T",
		"--title", "-t",
	},
	[]string{"--editor", "--web", "--help"},
))

var apiFlags = flagSet(
	[]string{
		"--cache", "--header", "-H", "--hostname", "--jq", "-q", "--method", "-X",
		"--preview", "-p", "--raw-field", "-f", "--field", "-F", "--input",
		"--template", "-t",
	},
	[]string{"--include", "-i", "--paginate", "--silent", "--slurp", "--verbose", "--help", "-h"},
)

func parse(args []string, repository string) (intent, error) {
	if len(args) == 1 {
		switch args[0] {
		case "--version", "version":
			return readIntent("gh --version"), nil
		case "--help", "-h":
			return readIntent("gh help"), nil
		}
	}
	if len(args) > 0 && args[0] == "help" {
		for _, argument := range args[1:] {
			if strings.HasPrefix(argument, "-") {
				return intent{}, fmt.Errorf("unrecognized flag %q for gh help", argument)
			}
		}
		return readIntent("gh help"), nil
	}

	position := 0
	var err error
	repository, position, err = consumeFlags(args, position, globalFlags, repository)
	if err != nil {
		return intent{}, err
	}
	if position >= len(args) {
		return intent{}, fmt.Errorf("no gh subcommand was provided")
	}
	if args[position] == "--help" || args[position] == "-h" {
		if position+1 != len(args) {
			return intent{}, fmt.Errorf("unexpected argument %q after gh help", args[position+1])
		}
		return readIntent("gh help"), nil
	}
	if args[position] == "api" {
		return parseAPI(args[position+1:])
	}
	if args[position] == "issue" && position+1 < len(args) && args[position+1] == "create" {
		return parseIssueCreate(args, position+2, repository)
	}

	command, argumentStart := readCommand(args, position)
	spec, recognized := readCommands[command]
	if !recognized {
		return intent{}, fmt.Errorf("unrecognized gh subcommand %q", strings.Join(args[position:argumentStart], " "))
	}
	if err := validateReadArguments(args[argumentStart:], spec); err != nil {
		return intent{}, fmt.Errorf("gh %s: %w", command, err)
	}
	return readIntent("gh " + command), nil
}

func parseIssueCreate(args []string, position int, repository string) (intent, error) {
	values := make(map[string][]string)
	repository, position, err := consumeCreateFlags(args, position, repository, values)
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
		Method: http.MethodPost, URL: githubIssueEndpoint(repository), Body: encoded,
		Kind: "create GitHub issue", Detail: describeIssueCreate(body),
	}, nil
}

func parseAPI(args []string) (intent, error) {
	values := make(map[string][]string)
	var endpoint string
	fieldFlag := ""
	explicitMethod := ""
	for position := 0; position < len(args); {
		argument := args[position]
		if !strings.HasPrefix(argument, "-") || argument == "-" {
			if endpoint != "" {
				return intent{}, fmt.Errorf("gh api has unexpected argument %q", argument)
			}
			endpoint = argument
			position++
			continue
		}
		name, inline, hasInline := splitFlag(argument)
		spec, known := apiFlags[name]
		if !known {
			return intent{}, fmt.Errorf("unrecognized flag %q for gh api", name)
		}
		position++
		value := ""
		if spec.takesValue {
			value = inline
			if !hasInline {
				if position >= len(args) {
					return intent{}, fmt.Errorf("flag %s requires a value", name)
				}
				value = args[position]
				position++
			}
			values[name] = append(values[name], value)
		} else if hasInline {
			return intent{}, fmt.Errorf("flag %s does not take a value", name)
		}
		switch name {
		case "--method", "-X":
			explicitMethod = strings.ToUpper(value)
		case "--raw-field", "-f", "--field", "-F", "--input":
			if fieldFlag == "" {
				fieldFlag = name
			}
		}
	}
	if endpoint == "" {
		return intent{}, fmt.Errorf("gh api endpoint is required")
	}
	if hasHelpFlag(args) {
		return readIntent("gh api help"), nil
	}

	method := explicitMethod
	if method == "" {
		method = http.MethodGet
		if fieldFlag != "" || endpoint == "graphql" {
			method = http.MethodPost
		}
	}
	apiURL, err := githubAPIEndpoint(endpoint)
	if err != nil {
		return intent{}, err
	}
	if method == http.MethodGet && fieldFlag == "" {
		return intent{
			Method: method, URL: apiURL, Kind: "read GitHub API",
			Detail: "gh api GET " + endpoint, Read: true,
		}, nil
	}

	bodyFields := make(map[string]any)
	for _, name := range []string{"--raw-field", "-f", "--field", "-F"} {
		for _, value := range values[name] {
			key, fieldValue, found := strings.Cut(value, "=")
			if !found || key == "" {
				return intent{}, fmt.Errorf("gh api field %q is not in key=value form", value)
			}
			bodyFields[key] = fieldValue
		}
	}
	body, _ := json.Marshal(bodyFields)
	reason := fmt.Sprintf("gh api uses %s", method)
	if fieldFlag != "" {
		reason += fmt.Sprintf(" because %s supplies request data", fieldFlag)
	}
	return intent{
		Method: method, URL: apiURL, Body: body, Kind: "call GitHub API",
		Detail: fmt.Sprintf("gh api %s %s", method, endpoint), Reason: reason,
	}, nil
}

func readCommand(args []string, position int) (string, int) {
	topLevel := args[position]
	switch topLevel {
	case "status", "browse":
		return topLevel, position + 1
	case "auth", "issue", "pr", "repo", "release", "run", "workflow", "project", "gist", "search":
		if position+1 >= len(args) {
			return topLevel, position + 1
		}
		return topLevel + " " + args[position+1], position + 2
	default:
		return topLevel, position + 1
	}
}

func validateReadArguments(args []string, spec readCommandSpec) error {
	positionals := 0
	for position := 0; position < len(args); {
		argument := args[position]
		if argument == "--" {
			positionals += len(args) - position - 1
			break
		}
		if !strings.HasPrefix(argument, "-") || argument == "-" {
			positionals++
			position++
			continue
		}
		name, _, hasInline := splitFlag(argument)
		flag, known := spec.flags[name]
		if !known {
			return fmt.Errorf("unrecognized flag %q", name)
		}
		position++
		if flag.takesValue && !hasInline {
			if position >= len(args) {
				return fmt.Errorf("flag %s requires a value", name)
			}
			position++
		} else if !flag.takesValue && hasInline {
			return fmt.Errorf("flag %s does not take a value", name)
		}
	}
	if positionals < spec.minPositionals {
		return fmt.Errorf("requires at least %d positional argument(s)", spec.minPositionals)
	}
	if spec.maxPositionals > 0 && positionals > spec.maxPositionals {
		return fmt.Errorf("accepts at most %d positional argument(s)", spec.maxPositionals)
	}
	return nil
}

func consumeFlags(
	args []string,
	position int,
	specs map[string]flagSpec,
	repository string,
) (string, int, error) {
	for position < len(args) && strings.HasPrefix(args[position], "-") {
		name, inline, hasInline := splitFlag(args[position])
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
		name, inline, hasInline := splitFlag(args[position])
		spec, known := issueCreateFlags[name]
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

func splitFlag(argument string) (name, value string, hasValue bool) {
	if name, value, hasValue = strings.Cut(argument, "="); hasValue {
		return name, value, true
	}
	for _, short := range []string{"-X", "-H", "-q", "-p", "-f", "-F", "-t", "-R"} {
		if strings.HasPrefix(argument, short) && len(argument) > len(short) {
			return short, strings.TrimPrefix(argument, short), true
		}
	}
	return argument, "", false
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

func githubAPIEndpoint(endpoint string) (*url.URL, error) {
	if strings.Contains(endpoint, "://") {
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return nil, fmt.Errorf("gh api endpoint %q is not a valid HTTPS URL", endpoint)
		}
		return parsed, nil
	}
	parsed, err := url.Parse("https://api.github.com/" + strings.TrimPrefix(endpoint, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse gh api endpoint: %w", err)
	}
	return parsed, nil
}

func readIntent(detail string) intent {
	endpoint, _ := url.Parse("https://api.github.com/")
	return intent{
		Method: http.MethodGet, URL: endpoint, Kind: "read GitHub data",
		Detail: detail, Read: true,
	}
}

func hasHelpFlag(args []string) bool {
	for _, argument := range args {
		if argument == "--help" || argument == "-h" {
			return true
		}
	}
	return false
}

func flagSet(values, booleans []string) map[string]flagSpec {
	result := make(map[string]flagSpec, len(values)+len(booleans))
	for _, name := range values {
		result[name] = flagSpec{takesValue: true}
	}
	for _, name := range booleans {
		result[name] = flagSpec{}
	}
	return result
}

func mergeFlagSets(sets ...map[string]flagSpec) map[string]flagSpec {
	result := make(map[string]flagSpec)
	for _, set := range sets {
		for name, spec := range set {
			result[name] = spec
		}
	}
	return result
}

func searchFlags(values ...string) map[string]flagSpec {
	return mergeFlagSets(commonReadFlags, flagSet(values, nil))
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
