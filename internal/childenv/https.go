package childenv

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// HTTPS returns a copy of base with network clients pointed at unring's
// loopback HTTP proxy and its CA trusted only by the child process. It never
// calls os.Setenv.
func HTTPS(base []string, proxyAddress, caCertificatePath string) ([]string, error) {
	host, _, err := net.SplitHostPort(proxyAddress)
	if err != nil {
		return nil, fmt.Errorf("split HTTPS proxy address: %w", err)
	}
	if !isLoopbackHost(host) {
		return nil, fmt.Errorf("refuse non-loopback HTTPS proxy address %q", proxyAddress)
	}
	proxyURL := (&url.URL{Scheme: "http", Host: proxyAddress}).String()
	baseValues := make(map[string]string)
	for _, entry := range base {
		key, value, found := strings.Cut(entry, "=")
		if found {
			baseValues[key] = value
		}
	}
	trustBundle, err := mergedTrustBundle(caCertificatePath, []string{
		baseValues["NODE_EXTRA_CA_CERTS"],
		baseValues["SSL_CERT_FILE"],
		baseValues["CURL_CA_BUNDLE"],
	})
	if err != nil {
		return nil, err
	}
	overrides := map[string]string{
		"HTTPS_PROXY":         proxyURL,
		"https_proxy":         proxyURL,
		"HTTP_PROXY":          proxyURL,
		"http_proxy":          proxyURL,
		"ALL_PROXY":           proxyURL,
		"all_proxy":           proxyURL,
		"FTP_PROXY":           proxyURL,
		"ftp_proxy":           proxyURL,
		"NODE_EXTRA_CA_CERTS": trustBundle,
		"SSL_CERT_FILE":       trustBundle,
		"CURL_CA_BUNDLE":      trustBundle,
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
		"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy",
		"ALL_PROXY", "all_proxy", "FTP_PROXY", "ftp_proxy",
		"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "CURL_CA_BUNDLE",
		"NO_PROXY", "no_proxy",
	} {
		result = append(result, key+"="+overrides[key])
	}
	return result, nil
}

func mergedTrustBundle(caCertificatePath string, inherited []string) (string, error) {
	uniquePaths := make([]string, 0, len(inherited))
	seen := make(map[string]struct{})
	for _, path := range inherited {
		if path == "" || path == caCertificatePath {
			continue
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		uniquePaths = append(uniquePaths, path)
	}
	if len(uniquePaths) == 0 {
		return caCertificatePath, nil
	}

	var bundle bytes.Buffer
	for _, path := range append(uniquePaths, caCertificatePath) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read inherited CA bundle %s: %w", path, err)
		}
		bundle.Write(data)
		if len(data) == 0 || data[len(data)-1] != '\n' {
			bundle.WriteByte('\n')
		}
	}
	sum := sha256.Sum256(bundle.Bytes())
	path := filepath.Join(
		filepath.Dir(caCertificatePath),
		fmt.Sprintf("merged-ca-%x.pem", sum[:12]),
	)
	if data, err := os.ReadFile(path); err == nil {
		if !bytes.Equal(data, bundle.Bytes()) {
			return "", fmt.Errorf("existing merged CA bundle %s has unexpected contents", path)
		}
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect merged CA bundle %s: %w", path, err)
	}

	temp, err := os.CreateTemp(filepath.Dir(caCertificatePath), ".merged-ca-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temporary merged CA bundle: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return "", fmt.Errorf("restrict temporary merged CA bundle: %w", err)
	}
	if _, err := temp.Write(bundle.Bytes()); err != nil {
		_ = temp.Close()
		return "", fmt.Errorf("write temporary merged CA bundle: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return "", fmt.Errorf("sync temporary merged CA bundle: %w", err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("close temporary merged CA bundle: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return "", fmt.Errorf("publish merged CA bundle: %w", err)
	}
	return path, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
