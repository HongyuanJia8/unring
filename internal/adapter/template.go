package adapter

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var templatePlaceholder = regexp.MustCompile(`\$\{([a-zA-Z0-9_.-]+)\}`)

// RenderUndo resolves a declared undo operation only from the original request
// and the real origin response. Missing values are errors: compensation must
// never guess a resource identifier.
func RenderUndo(
	declaration *Undo,
	request Request,
	responseBody []byte,
) (*Undo, error) {
	if declaration == nil {
		return nil, nil
	}
	values := map[string]any{
		"request": map[string]any{
			"method": request.Method,
			"url":    request.URL.String(),
			"host":   request.URL.Hostname(),
			"path":   request.URL.EscapedPath(),
			"body":   decodeBody(request.Header.Get("Content-Type"), request.Body),
		},
	}
	var response any
	if len(responseBody) > 0 {
		if err := json.Unmarshal(responseBody, &response); err != nil {
			return nil, fmt.Errorf("decode origin response for undo: %w", err)
		}
	}
	values["response"] = response

	render := func(input string) (string, error) {
		var renderErr error
		result := templatePlaceholder.ReplaceAllStringFunc(input, func(match string) string {
			if renderErr != nil {
				return ""
			}
			parts := strings.Split(templatePlaceholder.FindStringSubmatch(match)[1], ".")
			var current any = values
			for _, part := range parts {
				object, ok := current.(map[string]any)
				if !ok {
					renderErr = fmt.Errorf("undo placeholder %s does not name an object field", match)
					return ""
				}
				current, ok = object[part]
				if !ok {
					renderErr = fmt.Errorf("undo placeholder %s is unavailable", match)
					return ""
				}
			}
			switch value := current.(type) {
			case string:
				return value
			case float64, bool, nil:
				data, _ := json.Marshal(value)
				return string(data)
			default:
				data, err := json.Marshal(value)
				if err != nil {
					renderErr = fmt.Errorf("encode undo placeholder %s: %w", match, err)
					return ""
				}
				return string(data)
			}
		})
		return result, renderErr
	}

	rendered := cloneUndo(declaration)
	var err error
	if rendered.URL, err = render(rendered.URL); err != nil {
		return nil, err
	}
	parsedURL, err := url.ParseRequestURI(rendered.URL)
	if err != nil || !parsedURL.IsAbs() || parsedURL.Host == "" {
		return nil, fmt.Errorf("render undo URL: %w", err)
	}
	if rendered.Body, err = render(rendered.Body); err != nil {
		return nil, err
	}
	for name, value := range rendered.Headers {
		rendered.Headers[name], err = render(value)
		if err != nil {
			return nil, fmt.Errorf("render undo header %s: %w", name, err)
		}
	}
	return rendered, nil
}
