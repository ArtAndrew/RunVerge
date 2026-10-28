// SPDX-License-Identifier: Elastic-2.0
package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxStdioActive = 64

var errStream = errors.New("MCP stream violates bounded transport profile")

type guardedTransport struct {
	base    mcp.Transport
	timeout time.Duration
}

func (t *guardedTransport) SupportsProtocolVersion(version string) bool {
	return version == ProtocolVersion
}
func (t *guardedTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	base, err := t.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &guardedConnection{Connection: base, timeout: t.timeout, external: map[jsonrpc.ID]jsonrpc.ID{}, pending: map[jsonrpc.ID]pendingRequest{}}, nil
}

type pendingRequest struct {
	external  jsonrpc.ID
	cancelled bool
}
type guardedConnection struct {
	mcp.Connection
	timeout  time.Duration
	mu       sync.Mutex // Linearizes cancellation admission and the start of response writes.
	serial   int64
	external map[jsonrpc.ID]jsonrpc.ID
	pending  map[jsonrpc.ID]pendingRequest
}

func (c *guardedConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	for {
		message, err := c.Connection.Read(ctx)
		if err != nil {
			return nil, err
		}
		req, ok := message.(*jsonrpc.Request)
		if !ok {
			return nil, errStream
		}
		c.mu.Lock()
		copy := *req
		if req.ID.IsValid() {
			// Each admitted request receives a fresh internal generation. A cancelled
			// external ID may be reused without letting its old response cancel or
			// overwrite the newer request. Cancelled work still occupies pending capacity
			// until its bounded handler completes; malicious peers cannot grow tombstones.
			_, duplicate := c.external[req.ID]
			if duplicate || len(c.pending) >= maxStdioActive || c.serial >= 9007199254740991 {
				c.mu.Unlock()
				return nil, errStream
			}
			c.serial++
			internal, _ := jsonrpc.MakeID(float64(c.serial))
			c.external[req.ID] = internal
			c.pending[internal] = pendingRequest{external: req.ID}
			copy.ID = internal
		} else if req.Method == "notifications/cancelled" {
			var params map[string]json.RawMessage
			var value any
			// Invalid/unknown cancels are ignored without creating state.
			if json.Unmarshal(req.Params, &params) != nil || validWireID(params["requestId"]) != nil || json.Unmarshal(params["requestId"], &value) != nil {
				c.mu.Unlock()
				continue
			}
			external, e := jsonrpc.MakeID(value)
			internal, active := c.external[external]
			if e != nil || !active {
				c.mu.Unlock()
				continue
			}
			record := c.pending[internal]
			record.cancelled = true
			c.pending[internal] = record
			delete(c.external, external)
			params["requestId"], _ = json.Marshal(internal.Raw())
			copy.Params, _ = json.Marshal(params)
		}
		c.mu.Unlock()
		// The SDK still owns routing and cancellation delivery, using internal IDs.
		return &copy, nil
	}
}
func (c *guardedConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	response, ok := message.(*jsonrpc.Response)
	if !ok {
		return errStream
	} // No server requests/notifications in this profile.
	record, present := c.pending[response.ID]
	if !present {
		return nil
	}
	delete(c.pending, response.ID)
	if record.cancelled {
		return nil
	}
	if c.external[record.external] == response.ID {
		delete(c.external, record.external)
	}
	copy := *response
	copy.ID = record.external
	encoded, err := jsonrpc.EncodeMessage(&copy)
	if err != nil || len(encoded) > MaxResponseBytes {
		return errStream
	}
	// IOTransport checks context before writing, but a pipe write can then block.
	// Closing the underlying streams bounds it; the callback must not take c.mu.
	timer := time.AfterFunc(c.timeout, func() { _ = c.Connection.Close() })
	stop := context.AfterFunc(ctx, func() { _ = c.Connection.Close() })
	defer timer.Stop()
	defer stop()
	return c.Connection.Write(ctx, &copy)
}

// Validate exact outer names before Go's case-insensitive struct decoder. IDs
// are strings or safe JSON integers so cancellation's float64 conversion cannot
// target a different request. This is an explicit compatibility bound.
func validRequestFrame(raw []byte) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return errStream
	}
	for k := range fields {
		if k != "jsonrpc" && k != "id" && k != "method" && k != "params" {
			return errStream
		}
	}
	var version, method string
	if json.Unmarshal(fields["jsonrpc"], &version) != nil || version != "2.0" || json.Unmarshal(fields["method"], &method) != nil || method == "" {
		return errStream
	}
	if id, present := fields["id"]; present {
		if method == "notifications/cancelled" {
			return errStream
		}
		return validWireID(id)
	}
	if method == "notifications/cancelled" {
		return nil
	}
	return errStream
}
func validWireID(raw []byte) error {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if len(text) < 1 || len(text) > 128 {
			return errStream
		}
		return nil
	}
	integer, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || integer < -9007199254740991 || integer > 9007199254740991 {
		return errStream
	}
	return nil
}
