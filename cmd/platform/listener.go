// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"errors"
	"net"
)

func validateServeListener(address string, containerPreview, oidc, mcp, trustedProxy bool) error {
	if containerPreview {
		// This opt-in is for the single-owner Compose preview, whose published
		// host port is loopback-only. It does not relax identity/proxy profiles.
		if address != "0.0.0.0:8080" || oidc || mcp || trustedProxy {
			return errors.New("container preview requires 0.0.0.0:8080 and platform-token access without OIDC, HTTP MCP or a trusted TLS proxy")
		}
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("invalid listen address")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("development auth/fixture profile requires a loopback address")
	}
	return nil
}
