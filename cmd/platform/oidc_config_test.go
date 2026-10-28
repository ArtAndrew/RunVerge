// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOIDCConfigurationStrictPrivateFilesAndSeparatedKeys(t *testing.T) {
	dir := t.TempDir()
	seed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	if err := os.WriteFile(filepath.Join(dir, "seed"), []byte(seed+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config := `{"issuer":"https://idp.example.test","client_id":"platform","public_origin":"https://workspace.example.test","redirect_uri":"https://workspace.example.test/auth/oidc/callback","token_auth_method":"none","deployment_seed_ref":{"file":"seed"}}`
	path := filepath.Join(dir, "oidc.json")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, key, err := loadOIDCConfig(path)
	if err != nil || len(key) != 32 || len(cfg.CSRFKey) != 32 || bytes.Equal(key, cfg.CSRFKey) {
		t.Fatal("private config or domain-separated keys failed")
	}
	_, again, err := loadOIDCConfig(path)
	if err != nil || !bytes.Equal(key, again) {
		t.Fatal("deployment key derivation not stable")
	}
	for _, change := range []string{
		strings.Replace(config, `"client_id":"platform"`, `"client_id":"platform","client_id":"other"`, 1),
		strings.Replace(config, `"client_id"`, `"CLIENT_ID"`, 1),
		strings.Replace(config, `"file":"seed"`, `"file":"seed","environment":"AGENT_PLATFORM_OIDC_SEED"`, 1),
		strings.Replace(config, `"file":"seed"`, `"environment":"HOME"`, 1),
		strings.Replace(config, `"client_id":"platform"`, `"client_id":null`, 1),
		strings.Replace(config, `"token_auth_method":"none"`, `"client_secret":"plaintext","token_auth_method":"none"`, 1),
		config + config,
	} {
		if err := os.WriteFile(path, []byte(change), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadOIDCConfig(path); err == nil {
			t.Fatal("ambiguous/untrusted config accepted")
		}
	}
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadOIDCConfig(path); err == nil {
		t.Fatal("publicly readable configuration accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "seed"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadOIDCConfig(path); err == nil {
		t.Fatal("publicly readable secret accepted")
	}
	link := filepath.Join(dir, "alias.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadOIDCConfig(link); err == nil {
		t.Fatal("symlink configuration accepted")
	}
}

func TestOIDCConfigurationEnvironmentReferencesAreExplicit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oidc.json")
	t.Setenv("AGENT_PLATFORM_OIDC_SEED", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	t.Setenv("AGENT_PLATFORM_OIDC_CLIENT_SECRET", "fixture-private-value")
	config := `{"issuer":"https://idp.example.test","client_id":"platform","public_origin":"https://workspace.example.test","redirect_uri":"https://workspace.example.test/auth/oidc/callback","token_auth_method":"client_secret_basic","client_secret_ref":{"environment":"AGENT_PLATFORM_OIDC_CLIENT_SECRET"},"deployment_seed_ref":{"environment":"AGENT_PLATFORM_OIDC_SEED"}}`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadOIDCConfig(path)
	if err != nil || cfg.ClientSecret != "fixture-private-value" {
		t.Fatal("explicit environment reference failed")
	}
	t.Setenv("AGENT_PLATFORM_OIDC_CLIENT_SECRET", "")
	_, _, err = loadOIDCConfig(path)
	if err == nil || strings.Contains(err.Error(), "fixture-private-value") {
		t.Fatal("missing reference not safely rejected")
	}
}
