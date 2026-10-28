// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/storage"
)

// Service credentials are home-organization bound, never human credentials.
// The only raw-token output is a new private file, not stdout or an API result.
func runIssueServiceToken(ctx context.Context, s *storage.Store, args []string) error {
	f := flag.NewFlagSet("issue-service-token", flag.ContinueOnError)
	org := f.String("organization", "", "home organization ID")
	principal := f.String("principal", "", "existing active service principal ID")
	ceiling := f.String("role-ceiling", "viewer", "immutable maximum role: viewer or member")
	ttl := f.Duration("ttl", time.Hour, "whole-second key lifetime, subject to organization policy")
	reason := f.String("reason", "", "audit reason")
	out := f.String("out", "", "new private credential JSON file")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *org == "" || *principal == "" || *out == "" || *reason == "" || (*ceiling != "member" && *ceiling != "viewer") || *ttl < time.Second || *ttl > 24*time.Hour || *ttl%time.Second != 0 {
		return errors.New("issue-service-token requires organization, service principal, role ceiling, reason, whole-second ttl and new output file")
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0700); err != nil {
		return errors.New("cannot prepare private credential directory")
	}
	file, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("credential output must be a new private file")
	}
	saved := false
	defer func() {
		_ = file.Close()
		if !saved {
			_ = os.Remove(*out)
		}
	}()
	raw, metadata, err := s.IssueServiceToken(ctx, *org, *principal, *ceiling, *ttl, *reason)
	if err != nil {
		return errors.New("service credential issuance rejected; verify account, home organization, role and key-lifetime policy")
	}
	defer func() {
		if !saved {
			revokeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.RevokeIssuedServiceToken(revokeCtx, storage.Hash([]byte(raw)), "credential output failed"); err != nil {
				fmt.Fprintln(os.Stderr, "Service credential output failed and revocation could not be confirmed; trusted-host cleanup is required")
			}
		}
	}()
	data := struct {
		OrganizationID string            `json:"organization_id"`
		PrincipalID    string            `json:"principal_id"`
		Token          string            `json:"token"`
		Key            domain.ServiceKey `json:"key"`
	}{*org, *principal, raw, metadata}
	if json.NewEncoder(file).Encode(data) != nil || file.Sync() != nil || file.Close() != nil {
		return errors.New("cannot persist private service credentials; newly issued key is being revoked")
	}
	// Persist the new directory entry before reporting success. A failure leaves
	// issuance compensated instead of promising a durable credential file.
	dir, err := os.Open(filepath.Dir(*out))
	if err != nil {
		return errors.New("cannot confirm private credential directory durability; newly issued key is being revoked")
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil || closeErr != nil {
		return errors.New("cannot confirm private credential directory durability; newly issued key is being revoked")
	}
	saved = true
	fmt.Println("Service credential created; private file:", *out)
	return nil
}

// Recovery requires trusted host/database access and an explicit audit reason.
// It is never an unauthenticated network fallback when an identity provider is
// unavailable. Existing human consent and bearer credentials remain revoked.
func runRecoverLoginPolicy(ctx context.Context, s *storage.Store, args []string) error {
	f := flag.NewFlagSet("recover-login-policy", flag.ContinueOnError)
	org := f.String("organization", "", "organization ID")
	reason := f.String("reason", "", "explicit recovery audit reason")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *org == "" || *reason == "" {
		return errors.New("recover-login-policy requires organization and reason")
	}
	v, err := s.RecoverLoginPolicy(ctx, *org, *reason)
	if err != nil {
		return errors.New("login-policy recovery rejected; verify organization and current policy")
	}
	fmt.Printf("Login policy recovered to mixed mode; version %d. Issue fresh human credentials explicitly.\n", v.Version)
	return nil
}
