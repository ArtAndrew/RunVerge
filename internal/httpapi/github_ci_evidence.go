// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"agentplatform.local/platform/internal/webhookruntime"
)

const githubCIArchiveLimit = 4 << 20

var (
	errGitHubCIEvidence = errors.New("GitHub CI evidence unavailable")
	ciArchiveHost       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}\.(blob\.core\.windows\.net|actions\.githubusercontent\.com)$`)
	ciCredentialLine    = regexp.MustCompile(`(?i)github_pat_|gh[pousr]_|sk-or-v1-|authorization:|bearer[ \t]|[?&](?:sig|token|key)=|(?:password|secret|token|api[_-]?key)[ \t]*[:=]`)
)

func githubCIEvidenceClient(transport http.RoundTripper) *http.Client {
	if transport == nil {
		dialer := &net.Dialer{Timeout: 2 * time.Second}
		transport = &http.Transport{
			TLSClientConfig:    &tls.Config{MinVersion: tls.VersionTLS12},
			DisableCompression: true, MaxResponseHeaderBytes: 32 << 10,
			TLSHandshakeTimeout: 2 * time.Second, IdleConnTimeout: 30 * time.Second,
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(address)
				if err != nil || port != "443" || (host != "api.github.com" && !ciArchiveHost.MatchString(host)) {
					return nil, errGitHubCIEvidence
				}
				addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
				if err != nil || len(addresses) == 0 {
					return nil, errGitHubCIEvidence
				}
				for _, addr := range addresses {
					if !webhookruntime.AllowedReceiverAddress(addr, false) {
						return nil, errGitHubCIEvidence
					}
				}
				for _, addr := range addresses {
					if conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port)); err == nil {
						return conn, nil
					}
				}
				return nil, errGitHubCIEvidence
			},
		}
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func loadGitHubCIEvidence(ctx context.Context, client *http.Client, cfg GitHubCIConfig, event githubCIEvent) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	base := fmt.Sprintf("https://api.github.com/repos/%s/actions/runs/%d/attempts/%d", cfg.Repository, event.Run.ID, event.Run.Attempt)
	facts, err := getGitHubCI(ctx, client, base, cfg.ActionsReadToken)
	if err != nil {
		return "", err
	}
	raw, err := readGitHubCIBody(facts, 256<<10)
	var found githubCIRun
	if err != nil || json.Unmarshal(raw, &found) != nil ||
		found.ID != event.Run.ID || found.Attempt != event.Run.Attempt || found.WorkflowID != cfg.WorkflowID ||
		found.Repository.ID != cfg.RepositoryID || !strings.EqualFold(found.Repository.FullName, cfg.Repository) ||
		found.HeadRepository.ID != cfg.RepositoryID || found.HeadBranch != cfg.Branch || found.HeadSHA != event.Run.HeadSHA ||
		found.Event != event.Run.Event || found.Status != "completed" || found.Conclusion != event.Run.Conclusion {
		return "", errGitHubCIEvidence
	}
	redirect, err := getGitHubCI(ctx, client, base+"/logs", cfg.ActionsReadToken)
	if err != nil {
		return "", err
	}
	redirect.Body.Close()
	locations := redirect.Header.Values("Location")
	if redirect.StatusCode != http.StatusFound || len(locations) != 1 {
		return "", errGitHubCIEvidence
	}
	target, err := url.Parse(locations[0])
	if err != nil || target.Scheme != "https" || target.User != nil || target.Hostname() != target.Host ||
		!ciArchiveHost.MatchString(target.Host) || target.Fragment != "" || target.Path == "" {
		return "", errGitHubCIEvidence
	}
	// Explicitly build a fresh credential-free request. The URL itself is
	// temporary access material and must never enter a task, log or error.
	archive, err := getGitHubCI(ctx, client, target.String(), "")
	if err != nil {
		return "", err
	}
	raw, err = readGitHubCIBody(archive, githubCIArchiveLimit)
	if err != nil {
		return "", err
	}
	return githubCIExcerpt(raw, cfg.ActionsReadToken, cfg.ServiceToken, cfg.WebhookSecret)
}

func getGitHubCI(ctx context.Context, client *http.Client, endpoint, token string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errGitHubCIEvidence
	}
	request.Header.Set("Accept-Encoding", "identity")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, errGitHubCIEvidence
	}
	return response, nil
}

func readGitHubCIBody(response *http.Response, max int64) ([]byte, error) {
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > max || response.Header.Get("Content-Encoding") != "" {
		return nil, errGitHubCIEvidence
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, max+1))
	if err != nil || int64(len(raw)) > max {
		return nil, errGitHubCIEvidence
	}
	return raw, nil
}

func githubCIExcerpt(raw []byte, secrets ...string) (string, error) {
	files, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil || len(files.File) > 128 {
		return "", errGitHubCIEvidence
	}
	remaining := int64(githubCIArchiveLimit)
	for _, file := range files.File {
		if !file.Mode().IsRegular() || path.Ext(file.Name) != ".txt" {
			continue
		}
		if file.UncompressedSize64 > uint64(remaining) {
			return "", errGitHubCIEvidence
		}
		stream, err := file.Open()
		if err != nil {
			return "", errGitHubCIEvidence
		}
		body, err := io.ReadAll(io.LimitReader(stream, remaining+1))
		stream.Close()
		remaining -= int64(len(body))
		if err != nil || remaining < 0 {
			return "", errGitHubCIEvidence
		}
		if !utf8.Valid(body) || bytes.Contains(body, []byte("PRIVATE KEY-----")) {
			continue
		}
		if marker := bytes.Index(body, []byte("##[error]")); marker >= 0 {
			return formatGitHubCIExcerpt(body, marker, secrets), nil
		}
	}
	return "", errGitHubCIEvidence
}

func formatGitHubCIExcerpt(body []byte, marker int, secrets []string) string {
	start := bytes.LastIndexByte(body[:marker], '\n') + 1
	lineCount := 5
	for before := 0; before < 3 && start > 0; before++ {
		start = bytes.LastIndexByte(body[:start-1], '\n') + 1
		lineCount++
	}
	number := bytes.Count(body[:start], []byte{'\n'}) + 1
	rest := body[start:]
	var out strings.Builder
	fmt.Fprintf(&out, "\nLog file SHA-256: %x", sha256.Sum256(body))
	out.WriteString("\nLog excerpt (line numbers are local to this evidence):")
	for index := 0; index < lineCount && len(rest) > 0; index++ {
		raw, next, _ := bytes.Cut(rest, []byte{'\n'})
		rest = next
		line := strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
				return ' '
			}
			return r
		}, string(raw)))
		sensitive := ciCredentialLine.MatchString(line)
		for _, secret := range secrets {
			sensitive = sensitive || (secret != "" && strings.Contains(line, secret))
		}
		if sensitive {
			line = "[credential-like line omitted]"
		}
		if line == "" {
			line = "[empty]"
		}
		if len(line) > 360 {
			cut := 360
			for !utf8.RuneStart(line[cut]) {
				cut--
			}
			line = line[:cut] + "…"
		}
		fmt.Fprintf(&out, "\nL%d %s", number+index, line)
	}
	return out.String()
}
