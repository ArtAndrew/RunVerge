// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/installation"
	"agentplatform.local/platform/internal/storage"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// Set only by the native bundle builder; an ordinary source build stays explicitly
// development/unbound. These values contain no local paths or credentials.
var nativeVersion = "development"
var nativeSourceSHA256 = ""

func nativeIdentity() installation.Identity {
	return installation.Identity{APIVersion: domain.ContractVersion, OS: runtime.GOOS, Arch: runtime.GOARCH, SourceSHA256: nativeSourceSHA256, Migrations: storage.EmbeddedSchemaIdentity()}
}

func runVersion(args []string, out io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: platform version")
	}
	return json.NewEncoder(out).Encode(struct {
		Product   string                `json:"product"`
		Version   string                `json:"version"`
		GoVersion string                `json:"go_version"`
		Identity  installation.Identity `json:"identity"`
	}{"RunVerge", nativeVersion, runtime.Version(), nativeIdentity()})
}

func runBundle(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: platform bundle inspect|install|verify --source DIRECTORY --manifest-sha256 HASH [--destination NEW_DIRECTORY]")
	}
	command := args[0]
	if command != "inspect" && command != "install" && command != "verify" {
		return errors.New("unknown bundle command")
	}
	f := flag.NewFlagSet("bundle", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	source := f.String("source", "", "extracted bundle or installed directory")
	destination := f.String("destination", "", "new installation directory")
	hash := f.String("manifest-sha256", "", "SHA-256 obtained from a trusted release channel")
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || *source == "" || (command != "install" && *destination != "") || (command == "install" && *destination == "") || (command != "inspect" && *hash == "") {
		return errors.New("invalid bundle arguments")
	}
	var report installation.Report
	var err error
	if command == "install" {
		report, err = installation.Install(*source, *destination, *hash, nativeIdentity())
	} else {
		report, err = installation.Inspect(*source, *hash, nativeIdentity(), command == "verify")
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(report)
}

func nativeDefaultWeb() (string, error) {
	if nativeVersion == "development" {
		return "web/dist", nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", errors.New("cannot locate native installation")
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", errors.New("cannot locate native installation")
	}
	directory := filepath.Dir(filepath.Dir(executable))
	if _, err = installation.Inspect(directory, "", nativeIdentity(), true); err != nil {
		return "", errors.New("native installation is incomplete or changed; verify the installed bundle")
	}
	return filepath.Join(directory, "web"), nil
}
