// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/modelgateway"
	"agentplatform.local/platform/internal/modelruntime"
	"agentplatform.local/platform/internal/storage"
)

var usdAmount = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]{1,6})?$`)

// Exact USD units; reject excess precision instead of silently changing a budget.
func parseMicroUSD(value string) (int64, error) {
	if len(value) > 24 || !usdAmount.MatchString(value) {
		return 0, errors.New("USD amounts require a nonnegative decimal with at most six fractional digits")
	}
	parts := strings.SplitN(value, ".", 2)
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	integer, ok := new(big.Int).SetString(parts[0]+fraction+strings.Repeat("0", 6-len(fraction)), 10)
	if !ok || !integer.IsInt64() || integer.Int64() > domain.MaxMoney {
		return 0, errors.New("USD amount exceeds the supported limit")
	}
	return integer.Int64(), nil
}

func writeNewPrivateProfile(path string, value any) error {
	invalid := errors.New("output requires a new file in an existing private directory owned by you")
	parent, base := filepath.Dir(path), filepath.Base(path)
	before, err := os.Lstat(parent)
	if err != nil || !before.IsDir() || before.Mode().Perm()&0077 != 0 {
		return invalid
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return invalid
	}
	defer root.Close()
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		return invalid
	}
	info, ok := after.Sys().(*syscall.Stat_t)
	if !ok || info.Uid != uint32(os.Geteuid()) {
		return invalid
	}
	file, err := root.OpenFile(base, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return invalid
	}
	defer file.Close()
	if file.Chmod(0600) != nil {
		return invalid
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if encoder.Encode(value) != nil || file.Sync() != nil {
		return errors.New("profile was not completed; inspect the new private output file before continuing")
	}
	return file.Close()
}

// Preparation has no Store, credential lookup, catalog fetch or model request.
// Installation and selecting this model for a Task remain separate actions.
func runPrepareModel(args []string, out io.Writer) error {
	f := flag.NewFlagSet("prepare-model", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	provider := f.String("provider", "", "openrouter")
	organization := f.String("organization-id", "", "existing organization ID")
	id := f.String("id", "openrouter_text", "connection ID")
	version := f.Int("version", 1, "next immutable connection version")
	model := f.String("model", "", "exact provider/model slug; no automatic router")
	dataPolicy := f.String("data-collection", "deny", "deny or explicitly allow provider data collection; account restrictions still apply")
	account := f.String("provider-account", "", "non-secret label for your OpenRouter account")
	priceVersion := f.String("price-version", "", "operator's price revision label")
	inputRate := f.String("input-usd-per-million", "", "maximum USD per million input tokens")
	outputRate := f.String("output-usd-per-million", "", "maximum USD per million output tokens")
	fixedRate := f.String("fixed-usd-per-request", "0", "maximum fixed USD per request")
	callBudget := f.String("max-call-usd", "", "reservation ceiling for one request")
	inputTokens := f.Int64("max-input-tokens", 4096, "operator-assumed input token ceiling")
	outputTokens := f.Int("max-output-tokens", 256, "requested output token ceiling")
	path := f.String("out", "", "new JSON file in an existing private directory")
	if f.Parse(args) != nil || f.NArg() != 0 || *provider != "openrouter" || *path == "" ||
		!configurationCLIIdentifier.MatchString(*organization) || !configurationCLIIdentifier.MatchString(*id) ||
		*version < 1 || *version > 2147483647 || strings.TrimSpace(*account) == "" || strings.TrimSpace(*priceVersion) == "" {
		return errors.New("usage: platform prepare-model --provider openrouter --organization-id ORG --model PROVIDER/MODEL --provider-account LABEL --price-version LABEL --input-usd-per-million RATE --output-usd-per-million RATE --max-call-usd AMOUNT --out NEW_PRIVATE_JSON")
	}
	rates := make([]int64, 4)
	for i, raw := range []string{*inputRate, *outputRate, *fixedRate, *callBudget} {
		value, err := parseMicroUSD(raw)
		if err != nil {
			return err
		}
		rates[i] = value
	}
	profile := domain.ModelConnection{
		WireProfile: domain.ModelWireOpenRouterTextV1, AccountingMode: domain.ModelAccountingMetered,
		OpenRouterDataCollection: *dataPolicy,
		ID:                       *id, OrganizationID: *organization, Version: *version, Model: *model,
		ProviderAccount: *account, PriceVersion: *priceVersion,
		Endpoint: modelgateway.OpenRouterEndpoint, CredentialEnvironment: "PLATFORM_MODEL_KEY_OPENROUTER",
		MaxChargeMicroUSD: rates[3], MaxInputTokens: *inputTokens, MaxOutputTokens: *outputTokens,
		InputMicroUSDPerMillion: rates[0], OutputMicroUSDPerMillion: rates[1], FixedMicroUSD: rates[2],
		MaxInputBytes: 16384, MaxOutputBytes: 16384, TimeoutMilliseconds: 60000,
		TokenLimitField: "max_tokens", Active: true,
		BoundAssumptions: "Operator-selected rate caps and input-token assumption; OpenRouter price filters and output-token limits are requested. Bytes do not prove token counts. Taxes, credit purchase fees and final account billing are not measured. A provider response does not settle the reservation.",
	}
	if _, err := modelruntime.New(&storage.Store{}, storage.RuntimeModelConfig(profile, "offline-profile-validation-only")); err != nil {
		return errors.New("profile limits are invalid or the per-call reservation is smaller than the configured maximum token charge")
	}
	if err := writeNewPrivateProfile(*path, profile); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{"created": true, "file": *path,
		"provider": *provider, "model": *model, "installed": false, "model_requests": 0, "credentials_read": false})
}
