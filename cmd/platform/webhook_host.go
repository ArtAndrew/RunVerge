// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/storage"
)

var webhookKeyReference = regexp.MustCompile(`^PLATFORM_WEBHOOK_KEY_[A-Z0-9_]{1,64}$`)

// Generates a new private env_file, never stdout or PostgreSQL. Key generation
// does not need database credentials or grant a platform/receiver identity.
func runGenerateWebhookKey(args []string, out io.Writer) error {
	f := flag.NewFlagSet("generate-webhook-key", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	name := f.String("environment", "", "dedicated PLATFORM_WEBHOOK_KEY_* reference")
	path := f.String("out", "", "new private env_file")
	if f.Parse(args) != nil || f.NArg() != 0 || !webhookKeyReference.MatchString(*name) || *path == "" {
		return errors.New("usage: platform generate-webhook-key --environment PLATFORM_WEBHOOK_KEY_NAME --out NEW_PRIVATE_ENV_FILE")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return errors.New("secure key generation failed")
	}
	if err := os.MkdirAll(filepath.Dir(*path), 0700); err != nil {
		return errors.New("cannot create private key directory")
	}
	file, err := os.OpenFile(*path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("key output must be a new private file")
	}
	keep := false
	defer func() {
		file.Close()
		if !keep {
			_ = os.Remove(*path)
		}
	}()
	if _, err = io.WriteString(file, *name+"="+base64.RawURLEncoding.EncodeToString(key)+"\n"); err != nil {
		return errors.New("cannot write private key file")
	}
	if err = file.Sync(); err != nil {
		return errors.New("cannot persist private key file")
	}
	if err = file.Close(); err != nil {
		return errors.New("cannot close private key file")
	}
	keep = true
	_, err = fmt.Fprintln(out, "Webhook signing key created in the private file; install it in both sender and receiver environments")
	return err
}

func runWebhookHost(ctx context.Context, s *storage.Store, command string, args []string, out io.Writer) error {
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	if command == "install-webhook-destination" {
		path := f.String("file", "", "private approved receiver profile")
		if f.Parse(args) != nil || f.NArg() != 0 {
			return errors.New("usage: platform install-webhook-destination --file PRIVATE_PROFILE")
		}
		var p domain.WebhookDestinationProfile
		if readPrivateJSON(*path, &p) != nil {
			return errors.New("invalid private webhook destination profile")
		}
		if _, err := s.InstallWebhookDestination(ctx, p); err != nil {
			return errors.New("webhook destination installation rejected; inspect exact version and private profile")
		}
		_, err := fmt.Fprintln(out, "Webhook destination installed; no key was resolved and no network request was made")
		return err
	}
	if command != "retire-webhook-destination" {
		return errors.New("unknown webhook host command")
	}
	org := f.String("organization-id", "", "exact organization")
	id := f.String("id", "", "exact destination")
	version := f.Int("version", 0, "exact immutable version")
	if f.Parse(args) != nil || f.NArg() != 0 || !configurationCLIIdentifier.MatchString(*org) || !configurationCLIIdentifier.MatchString(*id) || *version < 1 || *version > 2147483647 {
		return errors.New("usage: platform retire-webhook-destination --organization-id ORG --id ID --version N")
	}
	if _, err := s.RetireWebhookDestination(ctx, *org, *id, *version); err != nil {
		return errors.New("webhook destination retirement rejected")
	}
	_, err := fmt.Fprintln(out, "Exact webhook destination retired; already-issued short permits and historical facts are preserved")
	return err
}
