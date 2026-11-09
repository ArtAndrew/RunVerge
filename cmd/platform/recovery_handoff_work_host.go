// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/storage"
)

type recoveryWorkScopeFile struct {
	Owners []struct {
		OrganizationID string `json:"organization_id"`
		DisplayName    string `json:"display_name"`
		OIDCIssuer     string `json:"oidc_issuer,omitempty"`
		OIDCSubject    string `json:"oidc_subject,omitempty"`
	} `json:"owners"`
}

// Saved before SQL. It is the only output containing passwords or tokens.
type recoveryWorkFile struct {
	Version            int                                  `json:"version"`
	TargetConfig       string                               `json:"target_config"`
	DatabaseOID        uint32                               `json:"database_oid"`
	RuntimeDatabaseURL string                               `json:"runtime_database_url"`
	BearerTokens       map[string]string                    `json:"bearer_tokens,omitempty"`
	Request            storage.RecoveryWorkAdmissionRequest `json:"request"`
}

func runRecoveryHandoffWorkHost(ctx context.Context, args []string, out io.Writer) error {
	usage := errors.New("usage: platform recovery handoff-enable --target-config FILE --peer-journal FILE --peer-trust FILE --peer-receipt FILE --scopes-file FILE --operator-reference REF --private-output NEW_FILE --confirm-current-authority; handoff-enable-inspect --private-output FILE")
	inspect := args[0] == "handoff-enable-inspect"
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	f.SetOutput(io.Discard)
	output := f.String("private-output", "", "create-only private credentials file")
	var targetPath, journalPath, trustPath, receiptPath, scopesPath, reference string
	var confirm bool
	if !inspect {
		f.StringVar(&targetPath, "target-config", "", "private provisioned target")
		f.StringVar(&journalPath, "peer-journal", "", "signed independent egress journal")
		f.StringVar(&trustPath, "peer-trust", "", "pinned observer key")
		f.StringVar(&receiptPath, "peer-receipt", "", "reviewed target receipt")
		f.StringVar(&scopesPath, "scopes-file", "", "explicit current owners and organizations")
		f.StringVar(&reference, "operator-reference", "", "current authority evidence reference")
		f.BoolVar(&confirm, "confirm-current-authority", false, "confirm independently verified current owners")
	}
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || !filepath.IsAbs(*output) ||
		(!inspect && (!confirm || !filepath.IsAbs(targetPath) || journalPath == "" || trustPath == "" || receiptPath == "" || scopesPath == "" || reference == "")) {
		return usage
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	var pending recoveryWorkFile
	if inspect {
		if readRecoveryFenceJSON(*output, &pending) != nil || !filepath.IsAbs(pending.TargetConfig) || pending.Version != 1 || pending.DatabaseOID == 0 || !pending.Request.Valid() {
			return storage.ErrRecoveryWorkAdmission
		}
		targetPath = pending.TargetConfig
	}
	var target recoveryHostTarget
	if readRecoveryFenceJSON(targetPath, &target) != nil {
		return storage.ErrRecoveryWorkAdmission
	}
	saved, err := loadProvisionedHandoffTarget(target)
	if err != nil || saved.Handoff.Retirement == nil || saved.Intent.WALTimeline == "" {
		return storage.ErrRecoveryWorkAdmission
	}
	if inspect {
		return inspectRecoveryWorkHost(ctx, target, saved, pending, out)
	}
	if target.DatabaseOID == 0 {
		return storage.ErrRecoveryWorkAdmission
	}
	h := saved.Handoff
	source, err := loadRecoveryHandoffSource(h.SourceIntentFile, h.SourceStopFile, h.BaselineDirectory, h.KeysFile, h.StagingParent,
		saved.ReceiptSHA256, target.HandoffPlanFile, targetPath, journalPath, trustPath, receiptPath, scopesPath, *output)
	if err != nil || captureHash(source.provenance) != saved.Intent.ProvenanceSHA256 {
		return storage.ErrRecoveryWorkAdmission
	}
	peer, err := recoveryarchive.VerifyHostRecoveryPeerJournal(journalPath, trustPath, target.TargetInstallationID, source.stop)
	var receipt recoveryHandoffPeerReceipt
	if err != nil || readRecoveryFenceJSON(receiptPath, &receipt) != nil || receipt.Version != 1 || !receipt.ExecutionHeld ||
		receipt.Scope != peer.Journal().Scope || receipt.JournalSHA256 != peer.SHA256() || receipt.SignerSHA256 != peer.SignerSHA256() ||
		receipt.TargetProvenanceSHA256 != saved.Intent.ProvenanceSHA256 || receipt.Scope.RestoreID != target.RestoreID {
		return storage.ErrRecoveryWorkAdmission
	}
	var scopes recoveryWorkScopeFile
	if readRecoveryFenceJSON(scopesPath, &scopes) != nil {
		return storage.ErrRecoveryWorkAdmission
	}
	pending, err = newRecoveryWorkFile(targetPath, target, receipt.TargetReviewSHA256, reference, scopes)
	if err != nil {
		return err
	}
	original, err := source.openOriginal(ctx)
	if err != nil {
		return err
	}
	defer original.Close()
	late, err := source.openBaseline(ctx)
	if err != nil {
		return err
	}
	defer late.Close()
	s, closeTarget, err := openBoundRecoveryHandoffTarget(ctx, target, saved)
	if err != nil {
		return err
	}
	defer closeTarget()
	pending.DatabaseOID = target.DatabaseOID
	client, err := source.openSourceObserver()
	if err != nil {
		return err
	}
	defer client.Close()
	if err = recoveryarchive.WriteHostJSONOutside(h.BaselineDirectory, *output, pending); err != nil {
		return err
	}
	admission, err := s.AdmitRecoveryWork(ctx, original, late, source.link, target.spec(), storage.DefaultRecoverySnapshotLimits(), peer, pending.Request,
		func(ctx context.Context) (recoveryarchive.CurrentRetiredSourceStop, error) {
			return recoveryarchive.RecheckHostRetiredSourceStop(ctx, client, source.stop, source.link)
		})
	if err != nil {
		return err
	}
	if errors.Join(original.Close(), late.Close()) != nil {
		return storage.ErrRecoveryWorkAdmissionUnknown
	}
	return json.NewEncoder(out).Encode(storage.RecoveryWorkInspection{State: "recorded", RequestSHA: admission.RequestSHA256, Admission: &admission})
}

func newRecoveryWorkFile(targetPath string, target recoveryHostTarget, reviewSHA, reference string, scopes recoveryWorkScopeFile) (recoveryWorkFile, error) {
	var empty recoveryWorkFile
	if len(scopes.Owners) == 0 || len(scopes.Owners) > 64 {
		return empty, storage.ErrRecoveryWorkAdmission
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return empty, storage.ErrRecoveryWorkAdmission
	}
	defer clear(random)
	password := base64.RawURLEncoding.EncodeToString(random)
	runtimeURL, err := recoveryWorkRuntimeURL(target, password)
	if err != nil {
		return empty, err
	}
	verifier, err := storage.RecoveryRuntimeVerifier(password)
	if err != nil {
		return empty, err
	}
	request := storage.RecoveryWorkAdmissionRequest{ActivationID: domain.NewID("activate"), TargetReviewSHA256: reviewSHA, OperatorReference: reference, RuntimeVerifier: verifier}
	tokens := make(map[string]string)
	oidcPrincipals := make(map[[2]string]string)
	for _, scope := range scopes.Owners {
		owner := storage.RecoveryWorkOwner{OrganizationID: scope.OrganizationID, PrincipalID: domain.NewID("usr"), DisplayName: scope.DisplayName,
			OIDCIssuer: scope.OIDCIssuer, OIDCSubject: scope.OIDCSubject}
		if owner.OIDCIssuer == "" && owner.OIDCSubject == "" {
			token := domain.NewID("ap") + domain.NewID("key")
			tokens[owner.OrganizationID] = token
			owner.TokenSHA256 = storage.Hash([]byte(token))
		} else {
			key := [2]string{owner.OIDCIssuer, owner.OIDCSubject}
			if id := oidcPrincipals[key]; id != "" {
				owner.PrincipalID = id
			} else {
				oidcPrincipals[key] = owner.PrincipalID
			}
		}
		request.Owners = append(request.Owners, owner)
	}
	sort.Slice(request.Owners, func(i, j int) bool { return request.Owners[i].OrganizationID < request.Owners[j].OrganizationID })
	if !request.Valid() {
		return empty, storage.ErrRecoveryWorkAdmission
	}
	return recoveryWorkFile{Version: 1, TargetConfig: targetPath, RuntimeDatabaseURL: runtimeURL, Request: request, BearerTokens: tokens}, nil
}

func recoveryWorkRuntimeURL(target recoveryHostTarget, password string) (string, error) {
	u, err := url.Parse(target.DatabaseURL)
	if err != nil || (u.Query().Get("sslmode") != "verify-full" && !net.ParseIP(u.Hostname()).IsLoopback()) {
		return "", storage.ErrRecoveryWorkAdmission
	}
	u.User = url.UserPassword("rvg_runtime_"+strings.TrimPrefix(target.RestoreID, "restore_"), password)
	return u.String(), nil
}

func inspectRecoveryWorkHost(ctx context.Context, target recoveryHostTarget, saved recoveryProvisionFile, pending recoveryWorkFile, out io.Writer) error {
	if target.DatabaseOID != pending.DatabaseOID {
		return storage.ErrRecoveryWorkAdmission
	}
	u, err := url.Parse(pending.RuntimeDatabaseURL)
	if err != nil || u.User == nil {
		return storage.ErrRecoveryWorkAdmission
	}
	password, present := u.User.Password()
	expected, err := recoveryWorkRuntimeURL(target, password)
	if err != nil || !present || expected != pending.RuntimeDatabaseURL || !storage.RecoveryRuntimePasswordMatches(password, pending.Request.RuntimeVerifier) {
		return storage.ErrRecoveryWorkAdmission
	}
	count := 0
	for _, owner := range pending.Request.Owners {
		if owner.TokenSHA256 != "" {
			token := pending.BearerTokens[owner.OrganizationID]
			if token == "" || storage.Hash([]byte(token)) != owner.TokenSHA256 {
				return storage.ErrRecoveryWorkAdmission
			}
			count++
		}
	}
	if count != len(pending.BearerTokens) {
		return storage.ErrRecoveryWorkAdmission
	}
	s, closeTarget, err := openBoundRecoveryHandoffTargetMode(ctx, target, saved, pending.DatabaseOID, false)
	if errors.Is(err, storage.ErrRecoveryProvision) {
		s, closeTarget, err = openBoundRecoveryHandoffTargetMode(ctx, target, saved, pending.DatabaseOID, true)
	}
	if err != nil {
		return err
	}
	defer closeTarget()
	result, err := s.InspectRecoveryWorkAdmission(ctx, target.spec(), pending.Request)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
