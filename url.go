package gitops

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ParseURL validates a repository URL and returns its normalized host and transport.
func ParseURL(raw string) (Repository, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, " \t\r\n") {
		return Repository{}, ErrInvalidURL
	}
	if strings.Contains(raw, "://") {
		return parseURL(raw)
	}
	return parseSCPURL(raw)
}

// ValidateHost allows any non-empty host when the allowlist is empty and exact normalized hosts otherwise.
func ValidateHost(host string, allowedHosts []string) error {
	host = normalizeHost(host)
	if host == "" {
		return ErrHostNotAllowed
	}
	if len(allowedHosts) == 0 {
		return nil
	}
	for _, allowed := range allowedHosts {
		allowedHost, allowedHasPort := hostWithoutPort(allowed)
		hostname, hasPort := hostWithoutPort(host)
		if normalizeHost(allowed) == host || (hasPort && !allowedHasPort && allowedHost == hostname) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrHostNotAllowed, host)
}

func hostWithoutPort(host string) (string, bool) {
	parsed, err := url.Parse("ssh://" + host)
	if err != nil || parsed.Hostname() == "" || parsed.Port() == "" {
		return normalizeHost(host), false
	}
	return normalizeHost(parsed.Hostname()), true
}

func parseURL(raw string) (Repository, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return Repository{}, ErrInvalidURL
	}
	transport := strings.ToLower(parsed.Scheme)
	if transport != "https" && transport != "ssh" {
		return Repository{}, ErrUnsupportedTransport
	}
	if parsed.User != nil {
		if transport == "https" {
			return Repository{}, ErrInvalidURL
		}
		if _, hasPassword := parsed.User.Password(); hasPassword {
			return Repository{}, ErrInvalidURL
		}
	}
	path := strings.Trim(parsed.Path, "/")
	if path == "" || strings.Contains(path, "\\") {
		return Repository{}, ErrInvalidURL
	}
	host := normalizeHost(parsed.Hostname())
	if parsed.Port() != "" {
		host = net.JoinHostPort(host, parsed.Port())
	}
	parsed.Scheme = transport
	parsed.Host = host
	parsed.Path = "/" + path
	return Repository{URL: parsed.String(), Host: host, Transport: transport, Path: path}, nil
}

func parseSCPURL(raw string) (Repository, error) {
	left, path, found := strings.Cut(raw, ":")
	user, host, hasUser := strings.Cut(left, "@")
	if !found || !hasUser || user == "" || host == "" || path == "" || strings.ContainsAny(raw, "\\?#") {
		return Repository{}, ErrInvalidURL
	}
	host = normalizeHost(host)
	return Repository{URL: fmt.Sprintf("%s@%s:%s", user, host, path), Host: host, Transport: "ssh", Path: path}, nil
}

func normalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}
