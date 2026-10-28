// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/domain"
)

func TestNativeBundleCommandsDoNotRequireDatabase(t *testing.T) {
	t.Setenv("DATABASE_URL", "private-invalid-database-do-not-connect")
	before := os.Args
	t.Cleanup(func() { os.Args = before })
	os.Args = []string{"platform", "version"}
	if err := run(); err != nil {
		t.Fatal("version accessed database", err)
	}
	os.Args = []string{"platform", "bundle", "inspect", "--source", t.TempDir()}
	if err := run(); err == nil || strings.Contains(err.Error(), "PostgreSQL") || strings.Contains(err.Error(), "private-invalid") {
		t.Fatal("bundle dispatch accessed database or leaked credentials", err)
	}
	os.Args = []string{"platform", "bundle", "unsupported"}
	if err := run(); err == nil || strings.Contains(err.Error(), "PostgreSQL") {
		t.Fatal("bundle argument handling accessed database", err)
	}
}

func TestNativeVersionDescribesEmbeddedInputsWithoutAppliedDatabaseClaim(t *testing.T) {
	var out bytes.Buffer
	if err := runVersion(nil, &out); err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(out.Bytes(), &object) != nil || len(object) != 4 {
		t.Fatal("invalid version output")
	}
	if !bytes.Contains(out.Bytes(), []byte(domain.ContractVersion)) || !bytes.Contains(out.Bytes(), []byte("migrations/021_webhook_delivery.sql")) || bytes.Contains(out.Bytes(), []byte("DATABASE_URL")) {
		t.Fatal("missing schema identity or leaked environment")
	}
	if err := runVersion([]string{"unexpected"}, &out); err == nil {
		t.Fatal("unexpected version flags accepted")
	}
}
