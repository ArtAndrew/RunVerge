// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"agentplatform.local/platform/internal/domain"
)

func prepareArgs(path string) []string {
	return []string{"--provider", "openrouter", "--organization-id", "org_test", "--model", "example/fixed-model:free", "--provider-account", "test-label", "--price-version", "operator-v1", "--input-usd-per-million", "0", "--output-usd-per-million", "0", "--max-call-usd", "0.000001", "--out", path}
}

func TestPrepareModelIsOfflinePrivateAndNeverOverwrites(t *testing.T) {
	t.Setenv("DATABASE_URL", "invalid-value-must-not-be-used")
	t.Setenv("PLATFORM_MODEL_KEY_OPENROUTER", "private-value-must-not-be-read-or-printed")
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "profile.json")
	var out bytes.Buffer
	if err := runPrepareModel(prepareArgs(path), &out); err != nil {
		t.Fatal(err)
	}
	var profile domain.ModelConnection
	if err := readPrivateJSON(path, &profile); err != nil {
		t.Fatal(err)
	}
	if profile.WireProfile != domain.ModelWireOpenRouterTextV1 || profile.MaxChargeMicroUSD != 1 || profile.AccountingMode != domain.ModelAccountingMetered || profile.MaxOutputTokens != 2048 {
		t.Fatal("prepared profile changed policy")
	}
	before, _ := os.ReadFile(path)
	if bytes.Contains(before, []byte("private-value")) || bytes.Contains(out.Bytes(), []byte("private-value")) {
		t.Fatal("credential disclosed")
	}
	if err := runPrepareModel(prepareArgs(path), &out); err == nil {
		t.Fatal("existing output replaced")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("existing output changed")
	}
	link := filepath.Join(directory, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := runPrepareModel(prepareArgs(link), &out); err == nil {
		t.Fatal("symlink output accepted")
	}
}

func TestPrepareModelRejectsMoneyAmbiguityAndInsufficientReserve(t *testing.T) {
	for _, value := range []string{"1e-6", "0.0000001", "-1", "NaN", "0.1\n", "01.0"} {
		if _, err := parseMicroUSD(value); err == nil {
			t.Fatal("ambiguous money accepted")
		}
	}
	if value, err := parseMicroUSD("1.234567"); err != nil || value != 1234567 {
		t.Fatal("exact money was rounded", err)
	}
	directory := t.TempDir()
	_ = os.Chmod(directory, 0700)
	path := filepath.Join(directory, "profile.json")
	args := prepareArgs(path)
	args = append(args, "--model", "example/fixed-model", "--input-usd-per-million", "1")
	if err := runPrepareModel(args, &bytes.Buffer{}); err == nil {
		t.Fatal("insufficient reserve accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("rejected profile created output")
	}
}
