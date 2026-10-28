// SPDX-License-Identifier: Elastic-2.0
package webhookruntime

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"agentplatform.local/platform/internal/domain"
)

var signingReference = regexp.MustCompile(`^PLATFORM_WEBHOOK_KEY_[A-Z0-9_]{1,64}$`)

// ValidateProfile performs no DNS or I/O and never resolves a signing key.
// Actual connection admission must recheck the resolved addresses and time.
func ValidateProfile(p domain.WebhookDestinationProfile) error {
	invalid := errors.New("invalid trusted webhook profile")
	d := p.Destination
	if !safeID.MatchString(d.ID) || !safeID.MatchString(d.OrganizationID) || d.Version < 1 || d.Version > 2147483647 || !safeID.MatchString(d.SigningKeyID) || !d.UnmeteredApproved || !signingReference.MatchString(p.SigningKeyEnvironment) || p.TimeoutSeconds < 1 || p.TimeoutSeconds > 30 || len(d.Name) == 0 || len(d.Name) > 120 || strings.TrimSpace(d.Name) == "" || !utf8.ValidString(d.Name) || strings.ContainsRune(d.Name, 0) {
		return invalid
	}
	u, err := url.Parse(p.Endpoint)
	if err != nil || len(p.Endpoint) > 2048 || strings.TrimSpace(p.Endpoint) != p.Endpoint || !utf8.ValidString(p.Endpoint) || strings.ContainsAny(p.Endpoint, "\x00\r\n\t #") || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(u.Hostname(), "%\\") {
		return invalid
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return invalid
		}
	}
	if len(p.TrustedCAPEM) > 65536 {
		return invalid
	}
	remaining := strings.TrimSpace(p.TrustedCAPEM)
	for remaining != "" {
		if !strings.HasPrefix(remaining, "-----BEGIN CERTIFICATE-----") {
			return invalid
		}
		block, rest := pem.Decode([]byte(remaining))
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return invalid
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return invalid
		}
		remaining = strings.TrimSpace(string(rest))
	}
	return nil
}

// Conservative receiver profile, not a universal routability classifier.
// IANA IPv4/IPv6 special-purpose registries were consulted on2026-10-26:
// https://www.iana.org/assignments/iana-ipv4-special-registry/
// https://www.iana.org/assignments/iana-ipv6-special-registry/
// Protocol, transition, documentation and shared ranges stay denied even when
// private LAN access is approved. Public special-purpose anycast is deliberately
// outside this first receiver profile. Real OS egress isolation remains separate.
var alwaysDeniedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.31.196.0/24"), netip.MustParsePrefix("192.52.193.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.175.48.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"), netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("100:0:0:1::/64"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("2620:4f:8000::/48"),
	netip.MustParsePrefix("3fff::/20"), netip.MustParsePrefix("5f00::/16"), netip.MustParsePrefix("fec0::/10"),
	// AWS documents this ULA metadata address; private opt-in does not admit it.
	// https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/instancedata-data-retrieval.html
	netip.MustParsePrefix("fd00:ec2::254/128"),
}

func AllowedReceiverAddress(ip netip.Addr, allowPrivate bool) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	if ip.IsLoopback() {
		return allowPrivate
	}
	for _, prefix := range alwaysDeniedPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	if ip.IsPrivate() {
		return allowPrivate
	}
	return ip.IsGlobalUnicast()
}
