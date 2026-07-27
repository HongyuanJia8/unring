package childenv

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// HTTPS returns a copy of base with HTTPS clients pointed at unring's loopback
// proxy and its CA trusted only by the child process. It never calls os.Setenv.
func HTTPS(base []string, proxyAddress, caCertificatePath string) ([]string, error) {
	host, _, err := net.SplitHostPort(proxyAddress)
	if err != nil {
		return nil, fmt.Errorf("split HTTPS proxy address: %w", err)
	}
	if !isLoopbackHost(host) {
		return nil, fmt.Errorf("refuse non-loopback HTTPS proxy address %q", proxyAddress)
	}
	proxyURL := (&url.URL{Scheme: "http", Host: proxyAddress}).String()
	overrides := map[string]string{
		"HTTPS_PROXY":         proxyURL,
		"https_proxy":         proxyURL,
		"NODE_EXTRA_CA_CERTS": caCertificatePath,
		"SSL_CERT_FILE":       caCertificatePath,
		"CURL_CA_BUNDLE":      caCertificatePath,
		// An inherited exclusion would be a silent coverage hole. Clear it for
		// the child; the proxy itself can still forward loopback destinations.
		"NO_PROXY": "",
		"no_proxy": "",
	}
	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if _, replaced := overrides[key]; replaced {
				continue
			}
		}
		result = append(result, entry)
	}
	for _, key := range []string{
		"HTTPS_PROXY", "https_proxy", "NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE",
		"CURL_CA_BUNDLE", "NO_PROXY", "no_proxy",
	} {
		result = append(result, key+"="+overrides[key])
	}
	return result, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
