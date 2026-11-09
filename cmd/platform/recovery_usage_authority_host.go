// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"path/filepath"
	"strings"
	"time"

	"agentplatform.local/platform/internal/recoveryaccess"
	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/storage"
)

func runRecoveryUsageAuthorityHost(ctx context.Context, args []string, out io.Writer) error {
	usage := errors.New("usage: platform recovery usage-access-init|usage-access-replace --authority-dir PRIVATE_DIRECTORY --intent-file PRIVATE_USAGE_PLAN --key-file PRIVATE_KEY --credential-reference REF --operator-reference REF --expires-at UTC --confirm-current-account [--expected-epoch N]; usage-access-inspect|usage-access-revoke|usage-access-resolve --authority-dir PRIVATE_DIRECTORY [command options]")
	if len(args) == 0 {
		return usage
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var authority, path, keyFile, credentialRef, operatorRef, expires, operation string
	var epoch uint64
	var confirmed bool
	f.StringVar(&authority, "authority-dir", "", "private usage authority directory")
	switch args[0] {
	case "usage-access-init", "usage-access-replace":
		f.StringVar(&path, "intent-file", "", "reviewed private usage plan")
		f.StringVar(&keyFile, "key-file", "", "private current provider key; never a command-line key")
		f.StringVar(&credentialRef, "credential-reference", "", "non-secret current key label")
		f.StringVar(&operatorRef, "operator-reference", "", "non-secret operator verification reference")
		f.StringVar(&expires, "expires-at", "", "explicit UTC expiry in RFC3339 format")
		f.BoolVar(&confirmed, "confirm-current-account", false, "attest current key/account mapping independently of the archives")
		if args[0] == "usage-access-replace" {
			f.Uint64Var(&epoch, "expected-epoch", 0, "exact inspected epoch")
		}
	case "usage-access-inspect":
	case "usage-access-revoke":
		f.Uint64Var(&epoch, "expected-epoch", 0, "exact inspected epoch")
		f.StringVar(&operatorRef, "operator-reference", "", "non-secret revocation reference")
		f.BoolVar(&confirmed, "confirm-revoke", false, "disable acquisition and new observation admission")
	case "usage-access-resolve":
		f.StringVar(&operation, "operation-id", "", "exact inspected pending operation")
	default:
		return usage
	}
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || authority == "" {
		return usage
	}
	configure := args[0] == "usage-access-init" || args[0] == "usage-access-replace"
	if configure && (!confirmed || path == "" || keyFile == "" || credentialRef == "" || operatorRef == "" || expires == "") {
		return usage
	}
	if args[0] == "usage-access-replace" && epoch == 0 {
		return usage
	}
	if args[0] == "usage-access-revoke" && (!confirmed || epoch == 0 || operatorRef == "") {
		return usage
	}
	if args[0] == "usage-access-resolve" && operation == "" {
		return usage
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	var err error
	authority, err = filepath.Abs(authority)
	if err != nil {
		return usage
	}
	emit := func(v any) error { return json.NewEncoder(out).Encode(v) }
	if !configure {
		info, err := recoveryaccess.InspectHost(ctx, authority)
		if err != nil {
			return err
		}
		if info.Current.Usage == nil || (info.Pending != nil && info.Pending.Usage == nil) {
			return errors.New("selected authority is not a usage authority")
		}
		switch args[0] {
		case "usage-access-inspect":
			return emit(info)
		case "usage-access-resolve":
			value, e := recoveryaccess.ResolveHost(ctx, authority, operation)
			if e != nil {
				return e
			}
			return emit(value)
		case "usage-access-revoke":
			policy := *info.Current.Usage
			policy.Enabled = false
			value, e := recoveryaccess.ReplaceUsageHost(ctx, authority, epoch, policy, "", operatorRef)
			if e != nil {
				return e
			}
			return emit(value)
		}
	}
	expiry, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil || !strings.HasSuffix(expires, "Z") || !expiry.After(time.Now()) {
		return errors.New("a future UTC usage expiry is required")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return usage
	}
	keyFile, err = filepath.Abs(keyFile)
	if err != nil {
		return usage
	}
	for _, input := range []string{path, keyFile} {
		check := recoveryarchive.RequireHostPathOutside
		if args[0] == "usage-access-init" {
			check = recoveryarchive.RequireHostPathOutsideNewDirectory
		}
		if check(authority, input) != nil {
			return errors.New("usage plan and input key must remain outside the authority directory")
		}
	}
	var saved recoveryUsageFile
	if readRecoveryFenceJSON(path, &saved) != nil || !recoveryUsageSavedValid(saved) || saved.Plan.State != "request_identity_available" {
		return errors.New("a supported private usage plan is required")
	}
	s, err := openRecoveryUsageSession(ctx, saved, path, path+".attempt", path+".acquisition", authority, keyFile)
	if err != nil {
		return err
	}
	defer s.close()
	current, err := s.store.PlanRecoveryUsageReadback(ctx, s.selected, s.late, s.link, s.target.spec(), storage.DefaultRecoverySnapshotLimits(), saved.Selection)
	if err != nil {
		return err
	}
	if current != saved.Plan {
		return errors.New("usage plan changed; review a new plan before configuring authority")
	}
	request := recoveryUsageRequest(current)
	if args[0] == "usage-access-replace" {
		info, e := recoveryaccess.InspectHost(ctx, authority)
		if e != nil {
			return e
		}
		if info.Current.Usage == nil || info.Current.Binding != request.Binding || info.Current.Epoch != epoch || info.Pending != nil {
			return errors.New("current usage authority does not match this target or epoch")
		}
	}
	// Close verified archives before touching credentials or writing authority.
	if err = s.close(); err != nil {
		return err
	}
	raw, err := recoveryarchive.ReadHostPrivateFile(keyFile)
	if err != nil {
		return errors.New("private current usage key unavailable")
	}
	defer clear(raw)
	key := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	policy := recoveryaccess.UsageHostPolicy{SourceID: request.SourceID, OrganizationID: request.OrganizationID, ModelConnectionID: request.ModelConnectionID,
		ModelConnectionVersion: request.ModelConnectionVersion, ConnectionIdentitySHA256: request.ConnectionIdentitySHA256, ArchivedProviderAccount: request.ArchivedProviderAccount,
		CredentialReference: credentialRef, ExpiresAt: expiry.UTC(), Enabled: true}
	var result recoveryaccess.HostInfo
	if args[0] == "usage-access-init" {
		result, err = recoveryaccess.InitUsageHost(ctx, authority, request.Binding, policy, key, operatorRef)
	} else {
		result, err = recoveryaccess.ReplaceUsageHost(ctx, authority, epoch, policy, key, operatorRef)
	}
	if err != nil {
		return err
	}
	return emit(result)
}
