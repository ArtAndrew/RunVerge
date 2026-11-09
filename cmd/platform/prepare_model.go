// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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
var taggedOllamaModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*:[A-Za-z0-9][A-Za-z0-9._-]*$`)

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
	provider := f.String("provider", "", "openrouter or ollama")
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
	outputTokens := f.Int("max-output-tokens", 2048, "requested output token ceiling, including model reasoning")
	ollamaPort := f.Int("port", 11434, "loopback port for a local Ollama server")
	ollamaTimeout := f.Int("timeout-seconds", 60, "time allowed for a local Ollama response, from 1 to 600 seconds")
	disableThinking := f.Bool("disable-thinking", false, "request answers without thinking on a compatible local Ollama model")
	localZeroTariff := f.Bool("local-zero-tariff", false, "assert Ollama runs the selected local weights with cloud routing disabled")
	path := f.String("out", "", "new JSON file in an existing private directory")
	openRouterUsage := errors.New("usage: platform prepare-model --provider openrouter --organization-id ORG --model PROVIDER/MODEL --provider-account LABEL --price-version LABEL --input-usd-per-million RATE --output-usd-per-million RATE --max-call-usd AMOUNT --out NEW_PRIVATE_JSON")
	ollamaUsage := errors.New("usage: platform prepare-model --provider ollama --local-zero-tariff --organization-id ORG --model NAME:TAG --out NEW_PRIVATE_JSON")
	if f.Parse(args) != nil || f.NArg() != 0 || *path == "" ||
		!configurationCLIIdentifier.MatchString(*organization) || !configurationCLIIdentifier.MatchString(*id) ||
		*version < 1 || *version > 2147483647 {
		if *provider == "ollama" {
			return ollamaUsage
		}
		return openRouterUsage
	}
	supplied := make(map[string]bool)
	f.Visit(func(option *flag.Flag) { supplied[option.Name] = true })
	profile := domain.ModelConnection{
		ID: *id, OrganizationID: *organization, Version: *version, Model: *model,
		MaxInputTokens: *inputTokens, MaxOutputTokens: *outputTokens,
		MaxInputBytes: 16384, MaxOutputBytes: 16384, TimeoutMilliseconds: 60000,
		TokenLimitField: "max_tokens", Active: true,
	}
	switch *provider {
	case "openrouter":
		if strings.TrimSpace(*account) == "" || strings.TrimSpace(*priceVersion) == "" ||
			supplied["port"] || supplied["timeout-seconds"] || supplied["disable-thinking"] || supplied["local-zero-tariff"] {
			return openRouterUsage
		}
		rates := make([]int64, 4)
		for i, raw := range []string{*inputRate, *outputRate, *fixedRate, *callBudget} {
			value, err := parseMicroUSD(raw)
			if err != nil {
				return err
			}
			rates[i] = value
		}
		profile.WireProfile = domain.ModelWireOpenRouterTextV1
		profile.AccountingMode = domain.ModelAccountingMetered
		profile.OpenRouterDataCollection = *dataPolicy
		profile.ProviderAccount, profile.PriceVersion = *account, *priceVersion
		profile.Endpoint, profile.CredentialEnvironment = modelgateway.OpenRouterEndpoint, "PLATFORM_MODEL_KEY_OPENROUTER"
		profile.InputMicroUSDPerMillion, profile.OutputMicroUSDPerMillion = rates[0], rates[1]
		profile.FixedMicroUSD, profile.MaxChargeMicroUSD = rates[2], rates[3]
		profile.BoundAssumptions = "Operator-selected rate caps and input-token assumption; OpenRouter price filters and output-token limits are requested. Bytes do not prove token counts. Taxes, credit purchase fees and final account billing are not measured. A provider response does not settle the reservation."
	case "ollama":
		if !*localZeroTariff || !taggedOllamaModel.MatchString(*model) || *ollamaPort < 1 || *ollamaPort > 65535 ||
			*ollamaTimeout < 1 || *ollamaTimeout > 600 {
			return ollamaUsage
		}
		tag := strings.ToLower((*model)[strings.LastIndex(*model, ":")+1:])
		if tag == "cloud" || strings.HasSuffix(tag, "-cloud") {
			return errors.New("the local zero-tariff profile requires locally stored model weights")
		}
		for _, option := range []string{"data-collection", "provider-account", "price-version", "input-usd-per-million", "output-usd-per-million", "fixed-usd-per-request", "max-call-usd"} {
			if supplied[option] {
				return errors.New("OpenRouter account and pricing options do not apply to the local Ollama profile")
			}
		}
		if !supplied["id"] {
			profile.ID = "local_ollama"
		}
		profile.AccountingMode = domain.ModelAccountingLocalUnmetered
		if *disableThinking {
			profile.WireProfile = domain.ModelWireOllamaNoThinkingV1
		}
		profile.ProviderAccount, profile.PriceVersion = "owned-local-model", "operator-zero-api-v1"
		profile.Endpoint = fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", *ollamaPort)
		profile.CredentialEnvironment = "PLATFORM_MODEL_KEY_LOCAL"
		profile.TimeoutMilliseconds = *ollamaTimeout * 1000
		profile.AllowInsecureLoopback = true
		profile.BoundAssumptions = "Operator declares zero API tariff for owned local inference with cloud routing disabled. Hardware and electricity costs are unmeasured. The input token ceiling is an operator assumption."
	default:
		return errors.New("prepare-model requires --provider openrouter or --provider ollama")
	}
	if _, err := modelruntime.New(&storage.Store{}, storage.RuntimeModelConfig(profile, "offline-profile-validation-only")); err != nil {
		if *provider == "ollama" {
			return errors.New("local model profile limits are invalid")
		}
		return errors.New("profile limits are invalid or the per-call reservation is smaller than the configured maximum token charge")
	}
	if err := writeNewPrivateProfile(*path, profile); err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(map[string]any{"created": true, "file": *path,
		"provider": *provider, "model": *model, "installed": false, "model_requests": 0, "credentials_read": false})
}
