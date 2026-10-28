// SPDX-License-Identifier: Elastic-2.0
package main

import "testing"

func TestContainerPreviewDoesNotRelaxDefaultOrIdentityListeners(t *testing.T) {
	for _, test := range []struct {
		name, address                      string
		preview, oidc, mcp, proxy, allowed bool
	}{
		{"default_loopback", "127.0.0.1:8080", false, false, false, false, true},
		{"default_ipv6_loopback", "[::1]:8080", false, false, false, false, true},
		{"wildcard_without_opt_in", "0.0.0.0:8080", false, false, false, false, false},
		{"explicit_container", "0.0.0.0:8080", true, false, false, false, true},
		{"different_container_port", "0.0.0.0:9000", true, false, false, false, false},
		{"arbitrary_host", "192.0.2.1:8080", true, false, false, false, false},
		{"oidc_needs_own_profile", "0.0.0.0:8080", true, true, false, false, false},
		{"mcp_needs_own_profile", "0.0.0.0:8080", true, false, true, false, false},
		{"proxy_needs_loopback", "0.0.0.0:8080", true, false, false, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validateServeListener(test.address, test.preview, test.oidc, test.mcp, test.proxy) == nil; got != test.allowed {
				t.Fatalf("allowed = %v, want %v", got, test.allowed)
			}
		})
	}
}
