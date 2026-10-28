// SPDX-License-Identifier: Elastic-2.0
package webhookruntime

import (
	"net/netip"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/domain"
)

func validProfileFixture() domain.WebhookDestinationProfile {
	return domain.WebhookDestinationProfile{Destination: domain.WebhookDestination{ID: "hooks_primary", OrganizationID: "org_fixture", Version: 1, Name: "Approved receiver", SigningKeyID: "key_v1", UnmeteredApproved: true, Active: true}, Endpoint: "https://hooks.example.invalid/events", SigningKeyEnvironment: "PLATFORM_WEBHOOK_KEY_FIXTURE", TimeoutSeconds: 5}
}

func TestWebhookProfileValidatesTrustedPrivateFieldsWithoutIO(t *testing.T) {
	if ValidateProfile(validProfileFixture()) != nil {
		t.Fatal("valid static receiver profile rejected")
	}
	for name, change := range map[string]func(*domain.WebhookDestinationProfile){
		"metered": func(p *domain.WebhookDestinationProfile) { p.Destination.UnmeteredApproved = false },
		"raw_key": func(p *domain.WebhookDestinationProfile) {
			p.SigningKeyEnvironment = "synthetic-secret-not-a-reference"
		},
		"other_purpose":          func(p *domain.WebhookDestinationProfile) { p.SigningKeyEnvironment = "PLATFORM_MODEL_KEY_FIXTURE" },
		"no_time_bound":          func(p *domain.WebhookDestinationProfile) { p.TimeoutSeconds = 0 },
		"large_time_bound":       func(p *domain.WebhookDestinationProfile) { p.TimeoutSeconds = 31 },
		"key_identity_injection": func(p *domain.WebhookDestinationProfile) { p.Destination.SigningKeyID = "key\nother" },
		"private_key_as_ca": func(p *domain.WebhookDestinationProfile) {
			p.TrustedCAPEM = "-----BEGIN PRIVATE KEY-----\nnever-accepted\n-----END PRIVATE KEY-----"
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := validProfileFixture()
			change(&p)
			if err := ValidateProfile(p); err == nil || strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatal("invalid private profile accepted or reflected")
			}
		})
	}
	for _, endpoint := range []string{"http://example.invalid/events", "https://user:password@example.invalid/events", "https://example.invalid/events?secret=value", "https://example.invalid/events?", "https://example.invalid/events#part", "https://example.invalid/events#", "https://example.invalid:0/events", "https://example.invalid:0443/events", "https://[fe80::1%25lo0]/events", "https://example.invalid/events\r\nInjected: yes"} {
		p := validProfileFixture()
		p.Endpoint = endpoint
		if err := ValidateProfile(p); err == nil || strings.Contains(err.Error(), endpoint) {
			t.Fatal("unapproved URL form accepted or reflected")
		}
	}
}

func TestWebhookReceiverAddressBlocksSpecialsEvenWithPrivateOptIn(t *testing.T) {
	for _, raw := range []string{"0.0.0.0", "0.0.0.1", "100.100.100.200", "169.254.169.254", "192.0.0.9", "192.0.2.1", "192.88.99.2", "198.19.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1", "255.255.255.255", "::", "fe80::1", "ff02::1", "64:ff9b::a9fe:a9fe", "64:ff9b:1::1", "100::1", "100:0:0:1::1", "2001::1", "2001:2::1", "2001:db8::1", "2002:a9fe:a9fe::1", "3fff::1", "5f00::1", "fec0::1", "fd00:ec2::254", "::ffff:169.254.169.254", "::ffff:100.100.100.200"} {
		ip := netip.MustParseAddr(raw)
		for _, private := range []bool{false, true} {
			if AllowedReceiverAddress(ip, private) {
				t.Fatalf("special-purpose or metadata address admitted: %s", raw)
			}
		}
	}
	if AllowedReceiverAddress(netip.Addr{}, true) || AllowedReceiverAddress(netip.MustParseAddr("fe80::1%en0"), true) {
		t.Fatal("invalid or zoned address admitted")
	}
}

func TestWebhookReceiverPrivatePolicyAndMappedAddresses(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1", "::1", "fd12:3456::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1"} {
		ip := netip.MustParseAddr(raw)
		if AllowedReceiverAddress(ip, false) || !AllowedReceiverAddress(ip, true) {
			t.Fatalf("explicit private opt-in not respected: %s", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "2001:4860:4860::8888", "::ffff:8.8.8.8"} {
		ip := netip.MustParseAddr(raw)
		if !AllowedReceiverAddress(ip, false) || !AllowedReceiverAddress(ip, true) {
			t.Fatalf("ordinary global address denied: %s", raw)
		}
	}
}
