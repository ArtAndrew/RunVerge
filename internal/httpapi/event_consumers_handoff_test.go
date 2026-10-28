// SPDX-License-Identifier: Elastic-2.0
package httpapi

// Explicit opt-in, isolated consumer fixture. Its private host-control files
// are test orchestration, not production HTTP endpoints or product features.

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/jsonwire"
	"agentplatform.local/platform/internal/storage"
)

type eventConsumerHandoffMetadata struct {
	BaseURL        string `json:"base_url"`
	OrganizationID string `json:"organization_id"`
	PrincipalID    string `json:"principal_id"`
	Token          string `json:"token"`
	TaskID         string `json:"task_id"`
	AgentID        string `json:"agent_id"`
	CAFile         string `json:"ca_file"`
	CommandFile    string `json:"command_file"`
	ReplyFile      string `json:"reply_file"`
	DoneFile       string `json:"done_file"`
}

type eventConsumerHandoffCommand struct {
	ID      string
	Command string
	TaskID  string
	After   *int64
}

type eventConsumerHandoffReply struct {
	ID        string                    `json:"id"`
	OK        bool                      `json:"ok"`
	ErrorCode string                    `json:"error_code,omitempty"`
	Snapshot  *domain.TaskEventSnapshot `json:"snapshot,omitempty"`
}

var eventHandoffID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func eventHandoffPrivate(info os.FileInfo, directory bool) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if directory {
		return info.IsDir() && info.Mode().Perm() == 0700
	}
	return info.Mode().IsRegular() && info.Mode().Perm() == 0600
}

func eventHandoffReadPrivate(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !eventHandoffPrivate(info, false) || info.Size() < 1 || info.Size() > 4096 {
		return nil, errors.New("unsafe or oversized private host-control file")
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(data) > 4096 {
		return nil, errors.New("private host-control read failed")
	}
	return data, nil
}

func eventHandoffPublish(path string, data []byte, replace bool) error {
	if info, err := os.Lstat(path); err == nil {
		if !replace || !eventHandoffPrivate(info, false) {
			return errors.New("private output already exists or is unsafe")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	temporary := path + "." + domain.NewID("tmp")
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(temporary)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if replace {
		return os.Rename(temporary, path)
	}
	// Link installs a complete file atomically without replacing an existing
	// target. The parent directory is private and on the same filesystem.
	return os.Link(temporary, path)
}

func eventHandoffDecode(data []byte) (eventConsumerHandoffCommand, error) {
	var command eventConsumerHandoffCommand
	invalid := errors.New("invalid private host command")
	if !jsonwire.ValidUnicode(data) {
		return command, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return command, invalid
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return command, invalid
		}
		seen[key] = true
		var value string
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil || bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &value) != nil {
			return command, invalid
		}
		switch key {
		case "id":
			command.ID = value
		case "command":
			command.Command = value
		case "task_id":
			command.TaskID = value
		case "after":
			after, err := strconv.ParseInt(value, 10, 64)
			if err != nil || after < 0 || strconv.FormatInt(after, 10) != value {
				return command, invalid
			}
			command.After = &after
		default:
			return command, invalid
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || decoder.Decode(&struct{}{}) != io.EOF || !eventHandoffID.MatchString(command.ID) || !eventHandoffID.MatchString(command.TaskID) || !seen["command"] {
		return command, invalid
	}
	if command.Command != "cancel" && command.Command != "advance" && command.Command != "revoke" || command.After != nil && command.Command != "advance" {
		return command, invalid
	}
	return command, nil
}

func eventHandoffExecute(ctx context.Context, s *storage.Store, principal domain.Principal, command eventConsumerHandoffCommand) eventConsumerHandoffReply {
	reply := eventConsumerHandoffReply{ID: command.ID}
	var exists bool
	if err := s.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM tasks WHERE organization_id=$1 AND id=$2)", principal.OrganizationID, command.TaskID).Scan(&exists); err != nil {
		reply.ErrorCode = "internal_error"
		return reply
	}
	if !exists {
		reply.ErrorCode = "not_found"
		return reply
	}
	var err error
	switch command.Command {
	case "cancel":
		_, err = s.CancelTask(ctx, principal, command.TaskID, domain.NewID("key"))
	case "advance":
		var after int64
		if command.After != nil {
			after = *command.After
		} else {
			var snapshot domain.TaskEventSnapshot
			snapshot, err = s.TaskEventSnapshot(ctx, principal, command.TaskID)
			after = snapshot.Window.ThroughSequence
		}
		if err == nil {
			_, err = s.AdvanceEventReplayWindow(ctx, principal.OrganizationID, command.TaskID, after, "Explicit isolated consumer smoke replay-window advance")
		}
	case "revoke":
		_, err = s.Pool.Exec(ctx, "UPDATE access_tokens SET revoked=true WHERE token_hash=$1 AND principal_id=$2", principal.TokenHash, principal.ID)
	}
	if err != nil {
		var known *domain.Error
		if errors.As(err, &known) {
			reply.ErrorCode = known.Code
		} else {
			reply.ErrorCode = "internal_error"
		}
		return reply
	}
	reply.OK = true
	if command.Command != "revoke" {
		// A committed host operation remains successful even if its optional
		// follow-up view is unavailable; never turn this into an implied replay.
		if snapshot, err := s.TaskEventSnapshot(ctx, principal, command.TaskID); err == nil {
			reply.Snapshot = &snapshot
		}
	}
	return reply
}

func TestEventConsumerHandoffFixture(t *testing.T) {
	metadataPath := os.Getenv("PLATFORM_EVENT_SMOKE_METADATA")
	if metadataPath == "" {
		t.Skip("explicit private consumer fixture opt-in required")
	}
	if !filepath.IsAbs(metadataPath) {
		t.Fatal("consumer metadata requires an absolute new private path")
	}
	parent, err := os.Lstat(filepath.Dir(metadataPath))
	if err != nil || !eventHandoffPrivate(parent, true) {
		t.Fatal("consumer fixture requires an owned private 0700 directory")
	}
	for _, path := range []string{metadataPath, metadataPath + ".ca.pem", metadataPath + ".command.json", metadataPath + ".reply.json", metadataPath + ".done"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("consumer fixture output/control paths must initially be absent")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()
	s := browserIntegrationStore(t)
	bootstrap, err := s.Bootstrap(ctx, "Isolated live event consumer fixture", 10000000)
	if err != nil {
		t.Fatal("isolated fixture bootstrap failed")
	}
	principal, err := s.Authenticate(ctx, bootstrap.Token, bootstrap.OrganizationID)
	if err != nil {
		t.Fatal("isolated fixture credential failed")
	}
	task := independentEventHTTPTask(t, s, bootstrap)
	staticDir, err := filepath.Abs(filepath.Join("..", "..", "web", "dist"))
	if err != nil {
		t.Fatal("compiled UI directory unavailable")
	}
	if info, err := os.Stat(filepath.Join(staticDir, "index.html")); err != nil || !info.Mode().IsRegular() {
		t.Fatal("build the current compiled UI before opting into its live fixture")
	}
	api := &Server{Store: s, StaticDir: staticDir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	server := httptest.NewUnstartedServer(api.Handler())
	server.Config.BaseContext = func(net.Listener) context.Context { return ctx }
	server.StartTLS()
	t.Cleanup(func() { server.CloseClientConnections(); server.Close() })
	metadata := eventConsumerHandoffMetadata{BaseURL: server.URL, OrganizationID: bootstrap.OrganizationID, PrincipalID: bootstrap.PrincipalID, Token: bootstrap.Token, TaskID: task.ID, AgentID: task.AgentID, CAFile: metadataPath + ".ca.pem", CommandFile: metadataPath + ".command.json", ReplyFile: metadataPath + ".reply.json", DoneFile: metadataPath + ".done"}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if eventHandoffPublish(metadata.CAFile, certificate, false) != nil {
		t.Fatal("private fixture CA publication failed")
	}
	body, _ := json.Marshal(metadata)
	if eventHandoffPublish(metadataPath, body, false) != nil {
		t.Fatal("private fixture metadata publication failed")
	}
	t.Log("isolated TLS event consumer fixture ready; metadata is private")
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	lastDigest := ""
	type remembered struct {
		digest string
		reply  eventConsumerHandoffReply
	}
	seen := map[string]remembered{}
	for {
		if _, err := eventHandoffReadPrivate(metadata.DoneFile); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal("unsafe private fixture completion file")
		}
		data, err := eventHandoffReadPrivate(metadata.CommandFile)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal("unsafe private fixture command file")
		}
		if err == nil && storage.Hash(data) != lastDigest {
			lastDigest = storage.Hash(data)
			command, decodeErr := eventHandoffDecode(data)
			reply := eventConsumerHandoffReply{ID: command.ID, ErrorCode: "invalid_request"}
			if decodeErr == nil {
				if previous, ok := seen[command.ID]; ok {
					if previous.digest == lastDigest {
						reply = previous.reply
					}
				} else if len(seen) < 128 {
					call, finish := context.WithTimeout(ctx, 5*time.Second)
					reply = eventHandoffExecute(call, s, principal, command)
					finish()
					seen[command.ID] = remembered{lastDigest, reply}
				}
			}
			body, _ := json.Marshal(reply)
			if eventHandoffPublish(metadata.ReplyFile, body, true) != nil {
				t.Fatal("private fixture acknowledgement publication failed")
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("isolated consumer fixture exceeded its 180-second bound")
		case <-ticker.C:
		}
	}
}
