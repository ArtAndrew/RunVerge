// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/storage"
)

type dedicatedBootstrapIntent struct {
	Name   string `json:"name"`
	Budget int64  `json:"budget_microusd"`
	Seed   string `json:"seed"`
}

func readBootstrapJSON(root *os.Root, name string, value any) error {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("invalid private bootstrap file")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) || info.Size() > 4096 {
		return errors.New("invalid private bootstrap file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(raw) > 4096 {
		return errors.New("invalid private bootstrap file")
	}
	return decodePrivateJSON(raw, value)
}

// The temporary file is complete and synced before it gets its final name.
func publishBootstrapJSON(root *os.Root, directory *os.File, name string, value any) error {
	return publishBootstrapFile(root, directory, name, func(w io.Writer) error {
		return json.NewEncoder(w).Encode(value)
	})
}

func publishBootstrapFile(root *os.Root, directory *os.File, name string, write func(io.Writer) error) error {
	temporary := domain.NewID(".bootstrap")
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	writeErr := write(file)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	if err := root.Link(temporary, name); err != nil {
		return err
	}
	return directory.Sync()
}

func runDedicatedBootstrap(ctx context.Context, store *storage.Store, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("bootstrap-dedicated", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("state-dir", "", "existing private installation state directory")
	name := flags.String("name", "", "organization name")
	budget := flags.Int64("budget-microusd", 100_000_000, "organization budget")
	if flags.Parse(args) != nil || flags.NArg() != 0 || !filepath.IsAbs(*path) ||
		strings.TrimSpace(*name) == "" || len(*name) > 120 || !utf8.ValidString(*name) ||
		strings.ContainsRune(*name, 0) || *budget <= 0 || *budget > domain.MaxMoney {
		return errors.New("usage: platform bootstrap-dedicated --state-dir PRIVATE_DIRECTORY --name NAME [--budget-microusd LIMIT]")
	}
	invalid := errors.New("dedicated bootstrap requires a private directory and its original state files")
	before, err := os.Lstat(*path)
	if err != nil || !before.IsDir() || before.Mode().Perm()&0077 != 0 {
		return invalid
	}
	owner, ok := before.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return invalid
	}
	root, err := os.OpenRoot(*path)
	if err != nil {
		return invalid
	}
	defer root.Close()
	directory, err := root.Open(".")
	if err != nil {
		return invalid
	}
	defer directory.Close()
	after, err := directory.Stat()
	if err != nil || !os.SameFile(before, after) {
		return invalid
	}
	if syscall.Flock(int(directory.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return errors.New("another dedicated bootstrap holds the installation state")
	}

	const intentName, credentialsName = "bootstrap-intent.json", "bootstrap.json"
	_, credentialsErr := root.Lstat(credentialsName)
	if credentialsErr != nil && !os.IsNotExist(credentialsErr) {
		return invalid
	}
	credentialsExist := credentialsErr == nil
	_, intentErr := root.Lstat(intentName)
	var intent dedicatedBootstrapIntent
	var seed [32]byte
	switch {
	case os.IsNotExist(intentErr) && !credentialsExist:
		if _, err = rand.Read(seed[:]); err != nil {
			return errors.New("cannot generate dedicated bootstrap seed")
		}
		intent = dedicatedBootstrapIntent{Name: *name, Budget: *budget, Seed: hex.EncodeToString(seed[:])}
		if err = publishBootstrapJSON(root, directory, intentName, intent); err != nil {
			return invalid
		}
	case intentErr == nil:
		if readBootstrapJSON(root, intentName, &intent) != nil {
			return invalid
		}
		n, decodeErr := hex.Decode(seed[:], []byte(intent.Seed))
		if decodeErr != nil || n != len(seed) || len(intent.Seed) != 2*len(seed) || directory.Sync() != nil {
			return invalid
		}
	default:
		return invalid
	}
	if intent.Name != *name || intent.Budget != *budget {
		return errors.New("dedicated bootstrap request differs from the saved intent")
	}
	credentials, err := store.BootstrapDedicated(ctx, *name, *budget, seed, credentialsExist)
	if err != nil {
		return errors.New("dedicated bootstrap could not confirm the original installation")
	}
	if credentialsExist {
		var saved storage.Bootstrap
		if readBootstrapJSON(root, credentialsName, &saved) != nil || saved != credentials {
			return invalid
		}
	} else if err = publishBootstrapJSON(root, directory, credentialsName, credentials); err != nil {
		return invalid
	}
	return json.NewEncoder(out).Encode(map[string]string{"organization_id": credentials.OrganizationID,
		"owner_principal_id": credentials.PrincipalID, "credentials_file": credentialsName})
}
