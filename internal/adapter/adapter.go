// Package adapter loads and evaluates declarative HTTP service adapters.
package adapter

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/google/cel-go/cel"
	"gopkg.in/yaml.v3"
)

// CEL was chosen over JSONPath because adapter rules need boolean logic across
// request metadata and bodies, not only value selection. CEL is deterministic,
// contains no network or LLM component, and lets us compile every expression at
// startup so a bad adapter fails before traffic can be classified differently.

const schemaVersion = 1

// Tier is the treatment assigned to a matched request.
type Tier string

const (
	TierStageable           Tier = "stageable"
	TierNeedsApproval       Tier = "needs-approval"
	TierAlreadyIrreversible Tier = "already-irreversible"
)

// Source is adapter YAML plus the filename used in diagnostics. Built-in and
// user adapters both enter the loader through this type and Load.
type Source struct {
	Name string
	Data []byte
}

// Adapter is the public, declarative adapter document.
type Adapter struct {
	Version int    `yaml:"version"`
	Name    string `yaml:"name"`
	Rules   []Rule `yaml:"rules"`
}

// Rule classifies the first matching request.
type Rule struct {
	Name           string             `yaml:"name"`
	Match          Match              `yaml:"match"`
	When           string             `yaml:"when,omitempty"`
	Tier           Tier               `yaml:"tier"`
	IdempotencyKey string             `yaml:"idempotency_key,omitempty"`
	Response       *SyntheticResponse `yaml:"response,omitempty"`
	Undo           *Undo              `yaml:"undo,omitempty"`
}

// Match identifies HTTP requests without any service-specific Go code.
type Match struct {
	Hosts   []string `yaml:"hosts"`
	Methods []string `yaml:"methods"`
	Path    string   `yaml:"path"`
}

// SyntheticResponse is returned for a staged call. It must identify itself as
// staged and must not invent resource IDs or claim an origin accepted the call.
type SyntheticResponse struct {
	Status  int               `yaml:"status"`
	Headers map[string]string `yaml:"headers,omitempty"`
	Body    string            `yaml:"body,omitempty"`
}

// Undo declares a best-effort compensating request and the honest user-facing
// boundary of that compensation.
type Undo struct {
	Method      string            `yaml:"method"`
	URL         string            `yaml:"url"`
	Headers     map[string]string `yaml:"headers,omitempty"`
	Body        string            `yaml:"body,omitempty"`
	Effect      string            `yaml:"effect,omitempty"`
	StillExists string            `yaml:"still_exists,omitempty"`
}

// Request is the complete deterministic classification input.
type Request struct {
	Method string
	URL    *url.URL
	Header http.Header
	Body   []byte
}

// Classification describes the adapter rule selected for a request.
type Classification struct {
	Tier           Tier
	Adapter        string
	Rule           string
	IdempotencyKey string
	Response       *SyntheticResponse
	Undo           *Undo
}

type compiledRule struct {
	adapter string
	rule    Rule
	when    cel.Program
	key     cel.Program
}

// Set is an ordered, fully validated collection of adapters.
type Set struct {
	adapters []Adapter
	rules    []compiledRule
}

//go:embed builtin/*.yaml
var builtinFiles embed.FS

// BuiltinSources returns the in-tree adapters as ordinary loader sources.
func BuiltinSources() ([]Source, error) {
	entries, err := builtinFiles.ReadDir("builtin")
	if err != nil {
		return nil, fmt.Errorf("read built-in adapter directory: %w", err)
	}
	sources := make([]Source, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		name := path.Join("builtin", entry.Name())
		data, err := builtinFiles.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("read built-in adapter %s: %w", name, err)
		}
		sources = append(sources, Source{Name: name, Data: data})
	}
	return sources, nil
}

// ReadFiles reads user adapter files into the same Source type used by built-ins.
// A single unreadable file aborts the whole operation and names the file.
func ReadFiles(paths []string) ([]Source, error) {
	sources := make([]Source, 0, len(paths))
	for _, filename := range paths {
		data, err := os.ReadFile(filename)
		if err != nil {
			return nil, fmt.Errorf("read adapter %s: %w", filename, err)
		}
		sources = append(sources, Source{Name: filename, Data: data})
	}
	return sources, nil
}

// Load parses, validates, and compiles all sources. There is intentionally no
// built-in name switch here: every source follows this exact public path.
func Load(sources ...Source) (*Set, error) {
	environment, err := cel.NewEnv(
		cel.Variable("request", cel.DynType),
		cel.Variable("body", cel.DynType),
	)
	if err != nil {
		return nil, fmt.Errorf("create adapter CEL environment: %w", err)
	}
	set := &Set{}
	adapterNames := make(map[string]string)
	for _, source := range sources {
		filename := source.Name
		if filename == "" {
			filename = "<adapter>"
		}
		document, err := decode(filename, source.Data)
		if err != nil {
			return nil, err
		}
		if previous, exists := adapterNames[document.Name]; exists {
			return nil, fmt.Errorf(
				"load adapter %s: adapter name %q is already defined by %s",
				filename, document.Name, previous,
			)
		}
		adapterNames[document.Name] = filename
		compiled, err := compileAdapter(filename, document, environment)
		if err != nil {
			return nil, err
		}
		set.adapters = append(set.adapters, document)
		set.rules = append(set.rules, compiled...)
	}
	return set, nil
}

// Adapters returns a detached copy of the loaded public documents.
func (set *Set) Adapters() []Adapter {
	if set == nil {
		return nil
	}
	result := make([]Adapter, len(set.adapters))
	copy(result, set.adapters)
	return result
}

// Classify returns the first matching adapter rule.
func (set *Set) Classify(input Request) (Classification, bool, error) {
	if set == nil || input.URL == nil {
		return Classification{}, false, nil
	}
	values := activation(input)
	for _, candidate := range set.rules {
		if !matches(candidate.rule.Match, input) {
			continue
		}
		if candidate.when != nil {
			value, _, err := candidate.when.Eval(values)
			if err != nil {
				return Classification{}, false, fmt.Errorf(
					"evaluate adapter %s rule %s condition: %w",
					candidate.adapter, candidate.rule.Name, err,
				)
			}
			condition, ok := value.Value().(bool)
			if !ok {
				return Classification{}, false, fmt.Errorf(
					"evaluate adapter %s rule %s condition: result is not boolean",
					candidate.adapter, candidate.rule.Name,
				)
			}
			if !condition {
				continue
			}
		}
		classification := Classification{
			Tier: candidate.rule.Tier, Adapter: candidate.adapter, Rule: candidate.rule.Name,
			Response: cloneResponse(candidate.rule.Response), Undo: cloneUndo(candidate.rule.Undo),
		}
		if candidate.key != nil {
			value, _, err := candidate.key.Eval(values)
			if err != nil {
				return Classification{}, false, fmt.Errorf(
					"evaluate adapter %s rule %s idempotency key: %w",
					candidate.adapter, candidate.rule.Name, err,
				)
			}
			key, ok := value.Value().(string)
			if !ok || strings.TrimSpace(key) == "" {
				return Classification{}, false, fmt.Errorf(
					"evaluate adapter %s rule %s idempotency key: result must be a non-empty string",
					candidate.adapter, candidate.rule.Name,
				)
			}
			classification.IdempotencyKey = key
		}
		return classification, true, nil
	}
	return Classification{}, false, nil
}

func decode(filename string, data []byte) (Adapter, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var document Adapter
	if err := decoder.Decode(&document); err != nil {
		return Adapter{}, fmt.Errorf("load adapter %s: decode YAML: %w", filename, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Adapter{}, fmt.Errorf("load adapter %s: multiple YAML documents are not allowed", filename)
		}
		return Adapter{}, fmt.Errorf("load adapter %s: decode trailing YAML: %w", filename, err)
	}
	return document, nil
}

func compileAdapter(filename string, document Adapter, environment *cel.Env) ([]compiledRule, error) {
	if document.Version != schemaVersion {
		return nil, fmt.Errorf(
			"load adapter %s: unsupported version %d (want %d)",
			filename, document.Version, schemaVersion,
		)
	}
	if strings.TrimSpace(document.Name) == "" {
		return nil, fmt.Errorf("load adapter %s: name is required", filename)
	}
	if len(document.Rules) == 0 {
		return nil, fmt.Errorf("load adapter %s: at least one rule is required", filename)
	}
	names := make(map[string]bool)
	compiled := make([]compiledRule, 0, len(document.Rules))
	for index, rule := range document.Rules {
		contextName := fmt.Sprintf("load adapter %s rule %d", filename, index+1)
		if strings.TrimSpace(rule.Name) == "" {
			return nil, fmt.Errorf("%s: name is required", contextName)
		}
		contextName = fmt.Sprintf("load adapter %s rule %q", filename, rule.Name)
		if names[rule.Name] {
			return nil, fmt.Errorf("%s: duplicate rule name", contextName)
		}
		names[rule.Name] = true
		if err := validateRule(rule); err != nil {
			return nil, fmt.Errorf("%s: %w", contextName, err)
		}
		item := compiledRule{adapter: document.Name, rule: rule}
		if rule.When != "" {
			program, err := compileExpression(environment, rule.When, cel.BoolType)
			if err != nil {
				return nil, fmt.Errorf("%s: compile when: %w", contextName, err)
			}
			item.when = program
		}
		if rule.IdempotencyKey != "" {
			program, err := compileExpression(environment, rule.IdempotencyKey, cel.StringType)
			if err != nil {
				return nil, fmt.Errorf("%s: compile idempotency_key: %w", contextName, err)
			}
			item.key = program
		}
		compiled = append(compiled, item)
	}
	return compiled, nil
}

func validateRule(rule Rule) error {
	if len(rule.Match.Hosts) == 0 {
		return errors.New("match.hosts must not be empty")
	}
	for _, host := range rule.Match.Hosts {
		if strings.TrimSpace(host) == "" || strings.ContainsAny(host, "/:") {
			return fmt.Errorf("invalid match host %q; use a hostname without scheme or port", host)
		}
	}
	if len(rule.Match.Methods) == 0 {
		return errors.New("match.methods must not be empty")
	}
	for _, method := range rule.Match.Methods {
		if method == "" || strings.ToUpper(method) != method {
			return fmt.Errorf("invalid method %q; methods must be uppercase", method)
		}
	}
	if rule.Match.Path == "" || !strings.HasPrefix(rule.Match.Path, "/") {
		return errors.New("match.path must be an absolute path glob")
	}
	if _, err := path.Match(rule.Match.Path, "/validation"); err != nil {
		return fmt.Errorf("invalid match.path glob: %w", err)
	}
	switch rule.Tier {
	case TierStageable, TierNeedsApproval:
	case TierAlreadyIrreversible:
		return errors.New(
			"tier already-irreversible is reserved for traffic unring could not intercept " +
				"and cannot be selected by an adapter",
		)
	default:
		return fmt.Errorf("tier %q is invalid", rule.Tier)
	}
	if rule.Tier == TierStageable {
		if rule.IdempotencyKey == "" {
			return errors.New("stageable rules require idempotency_key")
		}
		if rule.Response == nil {
			return errors.New("stageable rules require response")
		}
		if err := validateSyntheticResponse(*rule.Response); err != nil {
			return err
		}
	} else if rule.Response != nil {
		return errors.New("response is only valid for stageable rules")
	}
	if rule.Undo != nil {
		if rule.Undo.Method == "" || rule.Undo.URL == "" {
			return errors.New("undo.method and undo.url are required when undo is declared")
		}
		renderableURL := templatePlaceholder.ReplaceAllString(rule.Undo.URL, "placeholder")
		if templatePlaceholder.MatchString(rule.Undo.URL) &&
			templatePlaceholder.FindString(rule.Undo.URL) == rule.Undo.URL {
			renderableURL = "https://placeholder.invalid/resource"
		}
		parsedUndoURL, err := url.ParseRequestURI(renderableURL)
		if err != nil {
			return fmt.Errorf("invalid undo.url: %w", err)
		}
		if !parsedUndoURL.IsAbs() || parsedUndoURL.Host == "" {
			return errors.New("undo.url must be absolute")
		}
		for name := range rule.Undo.Headers {
			if strings.TrimSpace(name) == "" || isHopByHop(name) {
				return fmt.Errorf("undo header %q is invalid or hop-by-hop", name)
			}
		}
	}
	return nil
}

func validateSyntheticResponse(response SyntheticResponse) error {
	if response.Status < 200 || response.Status > 299 {
		return errors.New("stageable response.status must be a 2xx status")
	}
	stagedMarkers := 0
	for name, value := range response.Headers {
		if strings.EqualFold(name, "X-Unring-Staged") {
			stagedMarkers++
			if !strings.EqualFold(value, "true") {
				return fmt.Errorf(
					"stageable response header %q must be true", name,
				)
			}
		}
	}
	if stagedMarkers == 0 {
		return errors.New(`stageable response.headers must include "X-Unring-Staged: true"`)
	}
	if stagedMarkers > 1 {
		return errors.New(
			"stageable response.headers must not contain case-variant duplicate X-Unring-Staged headers",
		)
	}
	for name := range response.Headers {
		if isHopByHop(name) {
			return fmt.Errorf("stageable response header %q is hop-by-hop and not allowed", name)
		}
	}
	return nil
}

func compileExpression(environment *cel.Env, source string, expected *cel.Type) (cel.Program, error) {
	ast, issues := environment.Compile(source)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	if ast.OutputType() != expected && ast.OutputType() != cel.DynType {
		return nil, fmt.Errorf("expression has type %s, want %s", ast.OutputType(), expected)
	}
	program, err := environment.Program(ast)
	if err != nil {
		return nil, err
	}
	return program, nil
}

func matches(match Match, input Request) bool {
	host := strings.ToLower(input.URL.Hostname())
	hostMatched := false
	for _, pattern := range match.Hosts {
		pattern = strings.ToLower(pattern)
		if strings.HasPrefix(pattern, "*.") {
			suffix := strings.TrimPrefix(pattern, "*")
			hostMatched = strings.HasSuffix(host, suffix) && host != strings.TrimPrefix(suffix, ".")
		} else {
			hostMatched = host == pattern
		}
		if hostMatched {
			break
		}
	}
	if !hostMatched {
		return false
	}
	methodMatched := false
	for _, method := range match.Methods {
		if input.Method == method {
			methodMatched = true
			break
		}
	}
	if !methodMatched {
		return false
	}
	matched, _ := path.Match(match.Path, input.URL.EscapedPath())
	return matched
}

func activation(input Request) map[string]any {
	bodyHash := sha256.Sum256(input.Body)
	headers := make(map[string]any, len(input.Header))
	for name, values := range input.Header {
		headers[strings.ToLower(name)] = strings.Join(values, ",")
	}
	query := make(map[string]any, len(input.URL.Query()))
	for name, values := range input.URL.Query() {
		if len(values) == 1 {
			query[name] = values[0]
		} else {
			query[name] = append([]string(nil), values...)
		}
	}
	body := decodeBody(input.Header.Get("Content-Type"), input.Body)
	return map[string]any{
		"request": map[string]any{
			"method":      input.Method,
			"url":         input.URL.String(),
			"host":        input.URL.Hostname(),
			"path":        input.URL.EscapedPath(),
			"query":       query,
			"headers":     headers,
			"body_sha256": hex.EncodeToString(bodyHash[:]),
			"body_size":   int64(len(input.Body)),
		},
		"body": body,
	}
}

func decodeBody(contentType string, data []byte) any {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch mediaType {
	case "application/json", "application/graphql+json":
		var value any
		if json.Unmarshal(data, &value) == nil {
			return value
		}
	case "application/x-www-form-urlencoded":
		values, err := url.ParseQuery(string(data))
		if err == nil {
			result := make(map[string]any, len(values))
			for key, items := range values {
				if len(items) == 1 {
					result[key] = items[0]
				} else {
					result[key] = append([]string(nil), items...)
				}
			}
			return result
		}
	}
	return string(data)
}

func cloneResponse(response *SyntheticResponse) *SyntheticResponse {
	if response == nil {
		return nil
	}
	clone := *response
	clone.Headers = make(map[string]string, len(response.Headers))
	for name, value := range response.Headers {
		clone.Headers[name] = value
	}
	return &clone
}

func cloneUndo(undo *Undo) *Undo {
	if undo == nil {
		return nil
	}
	clone := *undo
	clone.Headers = make(map[string]string, len(undo.Headers))
	for name, value := range undo.Headers {
		clone.Headers[name] = value
	}
	return &clone
}

func isHopByHop(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	default:
		return false
	}
}
