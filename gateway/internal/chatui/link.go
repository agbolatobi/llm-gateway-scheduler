package chatui

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Link validates display-only destinations before any key is generated. The
// token is never added to the URL. A public UI requires HTTPS; local testing may
// use HTTP. This does not change the gateway's network or key-reuse configuration.
func Link(baseURL, override string) (string, error) {
	destination := override
	if destination == "" {
		destination = baseURL
	}
	parsed, err := url.Parse(destination)
	if err != nil || parsed.Opaque != "" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" || parsed.RawPath != "" {
		return "", fmt.Errorf("chat URL must be an HTTP(S) origin or /chat/ URL without credentials, query, or fragment")
	}
	if strings.ContainsAny(parsed.Host, " \t\r\n\\") || strings.HasSuffix(parsed.Host, ":") {
		return "", fmt.Errorf("chat URL has an invalid host")
	}
	if port := parsed.Port(); port != "" {
		number, portError := strconv.Atoi(port)
		if portError != nil || number < 1 || number > 65535 {
			return "", fmt.Errorf("chat URL has an invalid port")
		}
	}
	loopback := parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost" || parsed.Hostname() == "::1"
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
		return "", fmt.Errorf("chat URL requires HTTPS, except HTTP on a loopback host")
	}
	if parsed.Path != "" && parsed.Path != "/" && parsed.Path != "/chat" && parsed.Path != Path {
		return "", fmt.Errorf("chat URL must use the /chat/ path")
	}
	parsed.Path = Path
	return parsed.String(), nil
}
