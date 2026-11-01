// SPDX-License-Identifier: Elastic-2.0
package recoverycontrol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecoveryControlPrivateWitnessAndCancellation(t *testing.T) {
	// Keep the Unix path below the macOS socket length limit as well.
	root, err := os.MkdirTemp("/tmp", "rvg-control-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	path := filepath.Join(root, "owned", "witness.sock")
	entered, released := make(chan struct{}, 1), make(chan struct{}, 1)
	release := make(chan struct{})
	callback := func(ctx context.Context, source, nonce string) error {
		if source != "inst_"+strings.Repeat("a", 32) || nonce != strings.Repeat("b", 32) {
			t.Error("challenge changed")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("missing deadline")
		}
		entered <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			released <- struct{}{}
			return ctx.Err()
		}
	}
	server, err := Start(context.Background(), path, "api", callback)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	body := `{"source_id":"inst_` + strings.Repeat("a", 32) + `","nonce":"` + strings.Repeat("b", 32) + `"}`
	post := func(body string) *http.Response {
		t.Helper()
		r, e := client.Post("http://local/witness", "application/json", strings.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	for _, invalid := range []string{`{}`, body + `{}`, strings.TrimSuffix(body, "}") + `,"unexpected":true}`, strings.Repeat("x", 300)} {
		r := post(invalid)
		r.Body.Close()
		if r.StatusCode != 400 {
			t.Fatal("malformed input reached witness")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://local/witness", strings.NewReader(body))
	done := make(chan error, 1)
	go func() {
		r, e := client.Do(req)
		if r != nil {
			r.Body.Close()
		}
		done <- e
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("callback not reached")
	}
	conflict := post(body)
	conflict.Body.Close()
	if conflict.StatusCode != 409 {
		t.Fatal("concurrent witness admitted")
	}
	cancel()
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect did not cancel witness")
	}
	<-done
	close(release)
	r := post(body)
	defer r.Body.Close()
	var reply Reply
	if r.StatusCode != 200 || json.NewDecoder(r.Body).Decode(&reply) != nil || reply.PID != os.Getpid() || reply.Role != "api" || !noncePattern.MatchString(reply.BootID) {
		t.Fatal("missing live process identity")
	}
	// Re-creation may not adopt or remove the live endpoint.
	if _, err = Start(context.Background(), path, "api", callback); !errors.Is(err, ErrWitness) {
		t.Fatal("existing socket adopted")
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("existing socket removed")
	}
}

func TestRecoveryControlDoesNotExposeCallbackErrors(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "rvg-control-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	path := filepath.Join(root, "owned", "witness.sock")
	s, err := Start(context.Background(), path, "worker", func(context.Context, string, string) error { return errors.New("private-connection-secret") })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: time.Second}
	r, err := c.Post("http://local/witness", "application/json", strings.NewReader(`{"source_id":"inst_`+strings.Repeat("a", 32)+`","nonce":"`+strings.Repeat("b", 32)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	raw, _ := io.ReadAll(r.Body)
	if r.StatusCode != 503 || strings.Contains(string(raw), "private-connection-secret") {
		t.Fatal("private error leaked")
	}
	if err = os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err = Probe(context.Background(), path, "worker", "inst_"+strings.Repeat("a", 32), strings.Repeat("b", 32)); !errors.Is(err, ErrWitness) {
		t.Fatal("public socket accepted")
	}
}
