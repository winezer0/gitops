package gitops

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
)

func parseProxyURL(value string) (transport.ProxyOptions, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return transport.ProxyOptions{}, nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" {
		return transport.ProxyOptions{}, fmt.Errorf("invalid proxy URL")
	}
	proxy := transport.ProxyOptions{URL: parsed.Scheme + "://" + parsed.Host}
	if parsed.User != nil {
		proxy.Username = parsed.User.Username()
		proxy.Password, _ = parsed.User.Password()
	}
	if err := proxy.Validate(); err != nil {
		return transport.ProxyOptions{}, fmt.Errorf("invalid proxy URL")
	}
	return proxy, nil
}
