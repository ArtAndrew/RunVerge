// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agentplatform.local/platform/internal/storage"
)

// Trusted-host setup only. This command is deliberately absent from HTTP/MCP.
// Tokens identify a principal; the selected organization is a file context,
// not a claim that the global token is restricted to that organization.
func runIssueMemberToken(ctx context.Context, s *storage.Store, args []string) error {
	f := flag.NewFlagSet("issue-token", flag.ContinueOnError)
	org := f.String("organization", "", "active organization membership to validate")
	principal := f.String("principal", "", "existing active principal ID")
	out := f.String("out", "", "new private credential JSON file")
	ttl := f.Duration("ttl", time.Hour, "credential lifetime, whole seconds from 1s to 24h")
	if e := f.Parse(args); e != nil {
		return e
	}
	if f.NArg() != 0 || *org == "" || *principal == "" || *out == "" || *ttl < time.Second || *ttl > 24*time.Hour || *ttl%time.Second != 0 {
		return errors.New("issue-token requires organization, principal, new output file and whole-second ttl from 1s to 24h")
	}
	if e := os.MkdirAll(filepath.Dir(*out), 0700); e != nil {
		return errors.New("cannot prepare private credential directory")
	}
	file, e := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return errors.New("credential output must be a new private file")
	}
	saved := false
	defer func() {
		_ = file.Close()
		if !saved {
			_ = os.Remove(*out)
		}
	}()
	raw, expires, e := s.IssueMemberToken(ctx, *org, *principal, *ttl)
	if e != nil {
		return errors.New("credential issuance rejected; verify active organization membership")
	}
	// Failure after issuance revokes the newly generated credential. Do not use
	// the cancelled command context for this compensation.
	defer func() {
		if !saved {
			revokeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sum := sha256.Sum256([]byte(raw))
			if e := s.RevokeIssuedToken(revokeCtx, hex.EncodeToString(sum[:])); e != nil {
				fmt.Fprintln(os.Stderr, "Credential output failed and revocation could not be confirmed; trusted-host cleanup is required")
			}
		}
	}()
	data := struct {
		OrganizationID string    `json:"organization_id"`
		PrincipalID    string    `json:"principal_id"`
		Token          string    `json:"token"`
		ExpiresAt      time.Time `json:"expires_at"`
	}{*org, *principal, raw, expires}
	if json.NewEncoder(file).Encode(data) != nil || file.Sync() != nil || file.Close() != nil {
		return errors.New("cannot persist private credentials; newly issued credential is being revoked")
	}
	saved = true
	fmt.Println("Principal credential created; private file:", *out)
	return nil
}
