// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func ciTestArchive(t *testing.T, log string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create("test/04-build.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(f, log); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func ciTestFacts() githubCIRun {
	repo := githubCIRepository{ID: 10, FullName: "octo-org/sample"}
	return githubCIRun{
		ID: 100, WorkflowID: 20, Attempt: 1, Event: "push", Status: "completed", Conclusion: "failure",
		HeadBranch: "main", HeadSHA: "0123456789abcdef0123456789abcdef01234567", Repository: repo, HeadRepository: repo,
	}
}

func ciTestEvidenceTransport(t *testing.T, token string, runID int64, facts githubCIRun, location string, archive []byte, calls *atomic.Int64) http.RoundTripper {
	t.Helper()
	raw, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/repos/octo-org/sample/actions/runs/%d/attempts/1", runID)
	return independentCapabilitiesTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Scheme != "https" {
			t.Error("unexpected evidence request method or scheme")
		}
		reply := &http.Response{StatusCode: 200, Header: make(http.Header), Request: r}
		switch {
		case r.URL.Host == "api.github.com" && (r.URL.Path == path || r.URL.Path == path+"/logs"):
			if r.Header.Get("Authorization") != "Bearer "+token || r.URL.RawQuery != "" {
				t.Error("GitHub API request was not bound to the expected credential and attempt")
			}
			reply.Body = io.NopCloser(bytes.NewReader(raw))
			if r.URL.Path == path+"/logs" {
				reply.StatusCode = 302
				reply.Header.Set("Location", location)
			}
		case r.URL.String() == location && r.URL.Host == "testlogs.blob.core.windows.net":
			if r.Header.Get("Authorization") != "" || r.Header.Get("X-GitHub-Api-Version") != "" || r.Header.Get("Referer") != "" {
				t.Error("credential or origin metadata crossed the archive boundary")
			}
			reply.Body = io.NopCloser(bytes.NewReader(archive))
		default:
			t.Error("unexpected evidence destination")
			return nil, errGitHubCIEvidence
		}
		return reply, nil
	})
}

func TestGitHubCIEvidenceReadBoundary(t *testing.T) {
	facts := ciTestFacts()
	cfg := GitHubCIConfig{ActionsReadToken: "github_pat_" + strings.Repeat("a", 30), Repository: facts.Repository.FullName,
		RepositoryID: facts.Repository.ID, WorkflowID: facts.WorkflowID, Branch: facts.HeadBranch}
	event := githubCIEvent{Repository: facts.Repository, Run: facts}
	archive := ciTestArchive(t, "starting\nTOKEN=untrusted-value\n##[error] disk is full\nretry after cleanup\n")
	const validURL = "https://testlogs.blob.core.windows.net/temporary?sig=sensitive"
	var calls atomic.Int64
	read := func(facts githubCIRun, location string, archive []byte) (string, error) {
		calls.Store(0)
		return loadGitHubCIEvidence(context.Background(), githubCIEvidenceClient(
			ciTestEvidenceTransport(t, cfg.ActionsReadToken, event.Run.ID, facts, location, archive, &calls)), cfg, event)
	}
	excerpt, err := read(facts, validURL, archive)
	if err != nil || calls.Load() != 3 || !strings.Contains(excerpt, "L3 ##[error] disk is full") ||
		!strings.Contains(excerpt, "Log file SHA-256: ") ||
		!strings.Contains(excerpt, "L2 [credential-like line omitted]") || strings.Contains(excerpt, "untrusted-value") ||
		strings.Contains(excerpt, validURL) {
		t.Fatal("exact bounded excerpt was not returned")
	}
	for _, location := range []string{
		"http://testlogs.blob.core.windows.net/x", "https://127.0.0.1/x", "https://user@testlogs.blob.core.windows.net/x",
		"https://testlogs.blob.core.windows.net.evil.invalid/x", "https://testlogs.blob.core.windows.net:443/x",
	} {
		if _, err := read(facts, location, archive); err == nil || calls.Load() != 2 {
			t.Error("untrusted archive redirect was followed")
		}
	}
	for _, change := range []func(*githubCIRun){
		func(r *githubCIRun) { r.HeadSHA = strings.Repeat("b", 40) },
		func(r *githubCIRun) { r.Repository.ID++ },
		func(r *githubCIRun) { r.Attempt++ },
		func(r *githubCIRun) { r.HeadRepository.ID++ },
	} {
		other := facts
		change(&other)
		if _, err := read(other, validURL, archive); err == nil || calls.Load() != 1 {
			t.Error("metadata mismatch did not prevent archive access")
		}
	}
	for _, bad := range [][]byte{
		[]byte("not a ZIP"),
		ciTestArchive(t, strings.Repeat("x", githubCIArchiveLimit)+"##[error]"),
		ciTestArchive(t, "line with no diagnostic marker\n"),
	} {
		if _, err := read(facts, validURL, bad); err == nil || calls.Load() != 3 {
			t.Error("invalid or excessive archive yielded evidence")
		}
	}
}
