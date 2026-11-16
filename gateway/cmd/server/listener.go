package main

import (
	"fmt"
	"net"
	"strconv"
)

// listenerAddresses separates the validated local configuration from an optional
// container-facing socket. This never relaxes backend URL restrictions.
func listenerAddresses(address string, allowRemote bool) (string, string, error) {
	if address == "" {
		return "", "", nil
	}
	host, port, err := net.SplitHostPort(address)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || portNumber < 1 || portNumber > 65535 {
		return "", "", fmt.Errorf("listen requires a host and a port from 1 to 65535")
	}
	if host == "127.0.0.1" || host == "localhost" {
		return address, "", nil
	}
	if host == "0.0.0.0" && allowRemote {
		return net.JoinHostPort("127.0.0.1", port), address, nil
	}
	return "", "", fmt.Errorf("use loopback, or explicitly opt in with --listen 0.0.0.0:PORT --allow-remote-listen")
}
