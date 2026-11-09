// SPDX-License-Identifier: Elastic-2.0
// Package recoverycontrol provides a local, read-only witness from a live runtime.
// It has no task, login, migration, shutdown or recovery-clearance operations.
package recoverycontrol

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

const Socket = "/tmp/runverge-control/witness.sock"

var ErrWitness = errors.New("live runtime source witness not confirmed")
var noncePattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var sourcePattern = regexp.MustCompile(`^inst_[a-f0-9]{32}$`)

type Request struct {
	SourceID string `json:"source_id"`
	Nonce    string `json:"nonce"`
}
type Reply struct {
	SourceID string `json:"source_id"`
	Nonce    string `json:"nonce"`
	Role     string `json:"role"`
	PID      int    `json:"pid"`
	BootID   string `json:"boot_id"`
}
type Witness func(context.Context, string, string) error

type Server struct {
	http      *http.Server
	listener  net.Listener
	cancel    context.CancelFunc
	directory string
}

func (s *Server) Close() {
	s.cancel()
	_ = s.http.Close()
	_ = s.listener.Close()
	_ = os.Remove(s.directory)
}

func Start(ctx context.Context, path, role string, witness Witness) (*Server, error) {
	if witness == nil || (role != "api" && role != "worker") || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrWitness
	}
	directory := filepath.Dir(path)
	// Never remove/adopt a preexisting directory or socket, including stale ones.
	if err := os.Mkdir(directory, 0700); err != nil {
		stage := "witness directory creation failed"
		if errors.Is(err, os.ErrExist) {
			stage = "witness directory already exists"
		}
		if errors.Is(err, os.ErrPermission) {
			stage = "witness directory permission denied"
		}
		return nil, fmt.Errorf("%w: %s", ErrWitness, stage)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		_ = os.Remove(directory)
		return nil, fmt.Errorf("%w: witness socket listen failed", ErrWitness)
	}
	if os.Chmod(path, 0600) != nil {
		listener.Close()
		_ = os.Remove(directory)
		return nil, fmt.Errorf("%w: witness socket mode failed", ErrWitness)
	}
	secret := make([]byte, 16)
	if _, err = rand.Read(secret); err != nil {
		listener.Close()
		_ = os.Remove(directory)
		return nil, ErrWitness
	}
	boot := hex.EncodeToString(secret)
	lifetime, cancel := context.WithCancel(ctx)
	busy := make(chan struct{}, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/witness" || r.URL.RawQuery != "" {
			http.Error(w, "unavailable", 404)
			return
		}
		var request Request
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256))
		dec.DisallowUnknownFields()
		if dec.Decode(&request) != nil || dec.Decode(new(any)) != io.EOF || !sourcePattern.MatchString(request.SourceID) || !noncePattern.MatchString(request.Nonce) {
			http.Error(w, "invalid witness request", 400)
			return
		}
		select {
		case busy <- struct{}{}:
			defer func() { <-busy }()
		default:
			http.Error(w, "witness already active", 409)
			return
		}
		bounded, stop := context.WithTimeout(r.Context(), 22*time.Second)
		defer stop()
		if witness(bounded, request.SourceID, request.Nonce) != nil {
			http.Error(w, "source witness unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Reply{request.SourceID, request.Nonce, role, os.Getpid(), boot})
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 24 * time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 4096, BaseContext: func(net.Listener) context.Context { return lifetime }}
	s := &Server{server, listener, cancel, directory}
	go func() { _ = server.Serve(listener) }()
	return s, nil
}

// Probe talks only to a private Unix socket. It never reads DATABASE_URL or
// opens a substitute database connection on behalf of the running process.
func Probe(ctx context.Context, path, role, source, nonce string) (Reply, error) {
	var zero Reply
	if !sourcePattern.MatchString(source) || !noncePattern.MatchString(nonce) || (role != "api" && role != "worker") {
		return zero, ErrWitness
	}
	dir, err := os.Lstat(filepath.Dir(path))
	if err != nil || !dir.IsDir() || dir.Mode().Perm() != 0700 {
		return zero, ErrWitness
	}
	file, err := os.Lstat(path)
	if err != nil || file.Mode()&os.ModeSocket == 0 || file.Mode().Perm() != 0600 {
		return zero, ErrWitness
	}
	transport := &http.Transport{DisableKeepAlives: true, MaxResponseHeaderBytes: 4096, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", path)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return ErrWitness }}
	body, _ := json.Marshal(Request{source, nonce})
	req, err := http.NewRequestWithContext(ctx, "POST", "http://local/witness", bytes.NewReader(body))
	if err != nil {
		return zero, ErrWitness
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return zero, ErrWitness
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1025))
	if err != nil || len(data) > 1024 || response.StatusCode != 200 {
		return zero, ErrWitness
	}
	var reply Reply
	if json.Unmarshal(data, &reply) != nil || reply.SourceID != source || reply.Nonce != nonce || reply.Role != role || reply.PID != 1 || !noncePattern.MatchString(reply.BootID) {
		return zero, ErrWitness
	}
	return reply, nil
}
