// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"agentplatform.local/platform/internal/recoveryarchive"
	"github.com/jackc/pgx/v5"
)

var ErrRecoveryWorkAdmission = errors.New("restored work admission refused; inspect the source and reviewed target")
var ErrRecoveryWorkAdmissionUnknown = errors.New("work admission commit outcome unknown; inspect the exact target and saved credentials")

var recoveryActivationID = regexp.MustCompile(`^activate_[a-f0-9]{32}$`)
var recoveryWorkPrincipal = regexp.MustCompile(`^usr_[a-f0-9]{32}$`)
var recoveryWorkVerifier = regexp.MustCompile(`^SCRAM-SHA-256\$4096:[A-Za-z0-9+/]{22}==\$[A-Za-z0-9+/]{43}=:[A-Za-z0-9+/]{43}=$`)

const recoveryRuntimeConnectionLimit = 60

func recoveryRuntimeRoleName(restoreID string) string {
	return "rvg_runtime_" + strings.TrimPrefix(restoreID, "restore_")
}

type RecoveryWorkOwner struct {
	OrganizationID string `json:"organization_id"`
	PrincipalID    string `json:"principal_id"`
	DisplayName    string `json:"display_name"`
	TokenSHA256    string `json:"token_sha256,omitempty"`
	OIDCIssuer     string `json:"oidc_issuer,omitempty"`
	OIDCSubject    string `json:"oidc_subject,omitempty"`
}

type RecoveryWorkAdmissionRequest struct {
	ActivationID       string              `json:"activation_id"`
	TargetReviewSHA256 string              `json:"target_review_sha256"`
	OperatorReference  string              `json:"operator_reference"`
	Owners             []RecoveryWorkOwner `json:"owners"`
	RuntimeVerifier    string              `json:"runtime_verifier"`
}

type RecoveryWorkAdmission struct {
	ActivationID         string    `json:"activation_id"`
	TargetInstallationID string    `json:"target_installation_id"`
	RestoreID            string    `json:"restore_id"`
	DatabaseOID          uint32    `json:"database_oid"`
	RuntimeRole          string    `json:"runtime_role"`
	Organizations        []string  `json:"organizations"`
	ReviewSHA256         string    `json:"review_sha256"`
	RequestSHA256        string    `json:"request_sha256"`
	AdmittedAt           time.Time `json:"admitted_at"`
}

type RecoveryWorkInspection struct {
	State      string                 `json:"state"`
	RequestSHA string                 `json:"request_sha256"`
	Admission  *RecoveryWorkAdmission `json:"admission,omitempty"`
}

func (r RecoveryWorkAdmissionRequest) SHA256() string {
	return digestValue(struct {
		Profile string
		Request RecoveryWorkAdmissionRequest
	}{"runverge-work-admission-request-v1", r})
}

// RecoveryRuntimeVerifier accepts a freshly generated password in its
// 32-byte base64url encoding. The password never enters a SQL command.
func RecoveryRuntimeVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", ErrRecoveryWorkAdmission
	}
	return recoveryRuntimeVerifierForSalt(password, salt)
}

// Match the saved password against its original verifier without asking
// PostgreSQL to authenticate a potentially uncommitted login.
func RecoveryRuntimePasswordMatches(password, verifier string) bool {
	if !recoveryWorkVerifier.MatchString(verifier) {
		return false
	}
	_, tail, _ := strings.Cut(verifier, "4096:")
	encodedSalt, _, _ := strings.Cut(tail, "$")
	salt, err := base64.StdEncoding.Strict().DecodeString(encodedSalt)
	if err != nil {
		return false
	}
	recomputed, err := recoveryRuntimeVerifierForSalt(password, salt)
	return err == nil && hmac.Equal([]byte(recomputed), []byte(verifier))
}

func recoveryRuntimeVerifierForSalt(password string, salt []byte) (string, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(password)
	defer clear(raw)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != password {
		return "", ErrRecoveryWorkAdmission
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, 4096, sha256.Size)
	if err != nil {
		return "", ErrRecoveryWorkAdmission
	}
	defer clear(salted)
	mac := func(label string) []byte {
		h := hmac.New(sha256.New, salted)
		_, _ = h.Write([]byte(label))
		return h.Sum(nil)
	}
	client, server := mac("Client Key"), mac("Server Key")
	defer clear(client)
	defer clear(server)
	stored := sha256.Sum256(client)
	encode := base64.StdEncoding.EncodeToString
	return "SCRAM-SHA-256$4096:" + encode(salt) + "$" + encode(stored[:]) + ":" + encode(server), nil
}

func (r RecoveryHandoffTargetReview) SHA256() string { return digestValue(r) }

func (r RecoveryWorkAdmissionRequest) Valid() bool {
	if !recoveryActivationID.MatchString(r.ActivationID) || !validDigest(r.TargetReviewSHA256) || !validText(r.OperatorReference, 128) ||
		len(r.Owners) < 1 || len(r.Owners) > 64 || !recoveryWorkVerifier.MatchString(r.RuntimeVerifier) {
		return false
	}
	previous := ""
	identities := make(map[[2]string]string)
	principals := make(map[string]RecoveryWorkOwner)
	for _, o := range r.Owners {
		if !validText(o.OrganizationID, 128) || o.OrganizationID <= previous || !recoveryWorkPrincipal.MatchString(o.PrincipalID) || !validText(o.DisplayName, 120) {
			return false
		}
		if o.TokenSHA256 != "" {
			if !validDigest(o.TokenSHA256) || o.OIDCIssuer != "" || o.OIDCSubject != "" {
				return false
			}
		} else {
			if !validText(o.OIDCIssuer, 512) || !validText(o.OIDCSubject, 256) || !validOIDCIdentity(o.OIDCIssuer, o.OIDCSubject) {
				return false
			}
			key := [2]string{o.OIDCIssuer, o.OIDCSubject}
			if id := identities[key]; id != "" && id != o.PrincipalID {
				return false
			}
			identities[key] = o.PrincipalID
		}
		if p, exists := principals[o.PrincipalID]; exists &&
			(o.TokenSHA256 != "" || p.TokenSHA256 != "" || p.OIDCIssuer != o.OIDCIssuer || p.OIDCSubject != o.OIDCSubject || p.DisplayName != o.DisplayName) {
			return false
		}
		principals[o.PrincipalID] = o
		previous = o.OrganizationID
	}
	return true
}

// AdmitRecoveryWork rederives the exact reviewed state under the exclusive
// target gate. The source observer is checked while the same transaction still
// owns that gate. The trusted host must save the exact credentials first.
func (s *Store) AdmitRecoveryWork(ctx context.Context, selected, late *recoveryarchive.VerifiedArchive, link *recoveryarchive.VerifiedCaptureLink,
	spec RecoveryTargetSpec, limits RecoverySnapshotLimits, peer *recoveryarchive.VerifiedRecoveryPeerJournal, request RecoveryWorkAdmissionRequest,
	recheck func(context.Context) (recoveryarchive.CurrentRetiredSourceStop, error),
) (RecoveryWorkAdmission, error) {
	var empty RecoveryWorkAdmission
	if !limits.valid() || !request.Valid() || peer == nil || recheck == nil || s.recoveryProvision == nil ||
		s.recoveryProvision.intent.WALTimeline == "" {
		return empty, ErrRecoveryWorkAdmission
	}
	ctx, cancel := context.WithTimeout(ctx, limits.MaxDuration)
	defer cancel()
	original, baseline, err := s.bindRecoveryHandoffReview(ctx, selected, late, link, spec, limits, peer)
	if err != nil {
		return empty, err
	}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return empty, ErrRecoveryWorkAdmission
	}
	defer conn.Release()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return empty, ErrRecoveryWorkAdmission
	}
	defer recoveryFenceRollback(tx, conn.Conn())
	review, err := s.reviewRecoveryHandoffTargetTx(ctx, tx, selected, late, spec, limits, peer, original, baseline)
	if err != nil {
		return empty, err
	}
	result, err := s.prepareRecoveryWorkAdmissionTx(ctx, tx, spec, request, review)
	if err != nil {
		return empty, err
	}
	observing := time.Now()
	current, err := recheck(ctx)
	if err != nil || !current.Matches(review.PeerWindow.Scope.SourceStopSHA256, observing) {
		return empty, ErrRecoveryWorkAdmission
	}
	if err = commitRecoveryWorkAdmission(ctx, tx); err != nil {
		return empty, err
	}
	return result, nil
}

func commitRecoveryWorkAdmission(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		recoveryFenceClose(tx.Conn())
		return ErrRecoveryWorkAdmissionUnknown
	}
	return nil
}

// InspectRecoveryWorkAdmission waits for any in-flight activation under the
// same gate. An absent receipt is conclusive only on the exact still-closed
// target. The original request commitment remains valid after credential rotation.
func (s *Store) InspectRecoveryWorkAdmission(ctx context.Context, spec RecoveryTargetSpec, request RecoveryWorkAdmissionRequest) (RecoveryWorkInspection, error) {
	var empty RecoveryWorkInspection
	if !request.Valid() || s.recoveryProvision == nil || s.recoveryProvision.intent.WALTimeline == "" {
		return empty, ErrRecoveryWorkAdmission
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return empty, ErrRecoveryWorkAdmission
	}
	defer conn.Release()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return empty, ErrRecoveryWorkAdmission
	}
	defer recoveryFenceRollback(tx, conn.Conn())
	binding := s.recoveryProvision
	snapshot := binding.intent.SnapshotSHA256
	if _, _, err = s.evidenceTargetTx(ctx, tx, spec, snapshot); err != nil {
		return empty, err
	}
	if err = s.checkRecoveryProvisionImportTx(ctx, tx, spec, snapshot); err != nil {
		return empty, err
	}
	result := RecoveryWorkInspection{State: "not_recorded", RequestSHA: request.SHA256()}
	var admission RecoveryWorkAdmission
	var importSHA string
	err = tx.QueryRow(ctx, `SELECT activation_id,target_installation_id,restore_id,database_oid,runtime_role,organizations,review_sha256,
	 request_sha256,admitted_at,import_receipt_sha256 FROM `+pgx.Identifier{s.runtimeSchema, "recovery_work_admissions"}.Sanitize()).
		Scan(&admission.ActivationID, &admission.TargetInstallationID, &admission.RestoreID, &admission.DatabaseOID, &admission.RuntimeRole, &admission.Organizations,
			&admission.ReviewSHA256, &admission.RequestSHA256, &admission.AdmittedAt, &importSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		var stillClosed bool
		if tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_database WHERE oid=$1 AND datconnlimit=0)
		 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_roles WHERE rolname=$2)`, binding.databaseOID,
			recoveryRuntimeRoleName(spec.RestoreID)).Scan(&stillClosed) != nil || !stillClosed {
			return empty, ErrRecoveryWorkAdmission
		}
	} else {
		receipt, readErr := readRecoveryImportReceipt(ctx, tx, s.runtimeSchema)
		if err != nil || readErr != nil || !recoveryImportReceiptMatches(receipt, importSHA) || admission.ActivationID != request.ActivationID ||
			admission.RequestSHA256 != result.RequestSHA || admission.ReviewSHA256 != request.TargetReviewSHA256 ||
			admission.TargetInstallationID != spec.TargetInstallationID || admission.RestoreID != spec.RestoreID || admission.DatabaseOID != binding.databaseOID ||
			admission.RuntimeRole != recoveryRuntimeRoleName(spec.RestoreID) || !slices.Equal(admission.Organizations, recoveryWorkOrganizations(request.Owners)) {
			return empty, ErrRecoveryWorkAdmission
		}
		result.State, result.Admission = "recorded", &admission
	}
	if err = tx.Rollback(ctx); err != nil {
		return empty, ErrRecoveryWorkAdmission
	}
	return result, nil
}

func recoveryWorkOrganizations(owners []RecoveryWorkOwner) []string {
	orgs := make([]string, len(owners))
	for i, owner := range owners {
		orgs[i] = owner.OrganizationID
	}
	return orgs
}

func (s *Store) prepareRecoveryWorkAdmissionTx(ctx context.Context, tx pgx.Tx, spec RecoveryTargetSpec, request RecoveryWorkAdmissionRequest, review RecoveryHandoffTargetReview) (RecoveryWorkAdmission, error) {
	var empty RecoveryWorkAdmission
	if !request.Valid() || review.PeerWindow == nil || review.SHA256() != request.TargetReviewSHA256 ||
		review.Target.State != "held" || review.Target.RestoreID != spec.RestoreID || review.Target.InstallationID != spec.TargetInstallationID {
		return empty, ErrRecoveryWorkAdmission
	}
	for _, owner := range request.Owners {
		i := sort.Search(len(review.PeerWindow.Organizations), func(i int) bool {
			return review.PeerWindow.Organizations[i].OrganizationID >= owner.OrganizationID
		})
		if i == len(review.PeerWindow.Organizations) {
			return empty, ErrRecoveryWorkAdmission
		}
		o := review.PeerWindow.Organizations[i]
		if o.OrganizationID != owner.OrganizationID || !o.CoverageProven || o.FinancialHold {
			return empty, ErrRecoveryWorkAdmission
		}
	}
	if _, err := tx.Exec(ctx, "SET LOCAL synchronous_commit=on"); err != nil {
		return empty, ErrRecoveryWorkAdmission
	}
	if err := s.installRecoveryWorkOwnersTx(ctx, tx, request.Owners); err != nil {
		return empty, fmt.Errorf("%w (fresh owners)", err)
	}
	role, oid, err := s.createRecoveryRuntimeTx(ctx, tx, request.RuntimeVerifier)
	if err != nil {
		return empty, fmt.Errorf("%w (database login)", err)
	}
	p := review.PeerWindow
	admission := RecoveryWorkAdmission{ActivationID: request.ActivationID, TargetInstallationID: spec.TargetInstallationID,
		RestoreID: spec.RestoreID, DatabaseOID: review.DatabaseOID, RuntimeRole: role, Organizations: recoveryWorkOrganizations(request.Owners),
		ReviewSHA256: request.TargetReviewSHA256, RequestSHA256: request.SHA256()}
	err = tx.QueryRow(ctx, "INSERT INTO "+pgx.Identifier{s.runtimeSchema, "recovery_work_admissions"}.Sanitize()+`
	 (activation_id,restore_id,target_installation_id,database_oid,runtime_role,runtime_role_oid,organizations,review_sha256,request_sha256,
	  import_receipt_sha256,source_stop_sha256,retirement_seal_sha256,peer_journal_sha256,peer_signer_sha256,
	  accounting_head_id,accounting_head_ordinal,operator_reference)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17) RETURNING admitted_at`,
		admission.ActivationID, admission.RestoreID, admission.TargetInstallationID, admission.DatabaseOID, role, oid, admission.Organizations, admission.ReviewSHA256, admission.RequestSHA256,
		review.ImportReceiptSHA256, p.Scope.SourceStopSHA256, p.Scope.RetirementSealSHA256, p.JournalSHA256, p.SignerSHA256,
		review.AccountingHeadID, review.AccountingHeadOrdinal, request.OperatorReference).Scan(&admission.AdmittedAt)
	if err != nil {
		return empty, fmt.Errorf("%w (admission receipt)", ErrRecoveryWorkAdmission)
	}
	if err = s.installRecoveryOIDCRebindingsTx(ctx, tx, request); err != nil {
		return empty, fmt.Errorf("%w (SSO rebindings)", err)
	}
	return admission, nil
}

func (s *Store) installRecoveryWorkOwnersTx(ctx context.Context, tx pgx.Tx, owners []RecoveryWorkOwner) error {
	q := func(name string) string { return pgx.Identifier{s.runtimeSchema, name}.Sanitize() }
	// IAM triggers predate the recovery operator transaction's restricted
	// path. Resolve their relations in the catalog-verified application schema.
	if _, err := tx.Exec(ctx, "SET LOCAL search_path TO "+pgx.Identifier{s.runtimeSchema}.Sanitize()+",pg_catalog,pg_temp"); err != nil {
		return ErrRecoveryWorkAdmission
	}
	// Only these identity statement guards are bypassed for bootstrap. The
	// history guards remain installed and the table locks are transactional.
	tables := []string{"principals", "memberships", "access_tokens", "identity_mappings"}
	for _, table := range tables {
		if _, err := tx.Exec(ctx, "ALTER TABLE "+q(table)+" DISABLE TRIGGER recovery_runtime_write"); err != nil {
			return ErrRecoveryWorkAdmission
		}
	}
	created := make(map[string]bool)
	for _, owner := range owners {
		var mode, issuer string
		if tx.QueryRow(ctx, "SELECT q.mode,COALESCE(q.oidc_issuer,'') FROM "+q("organization_login_policies")+" q JOIN "+q("organizations")+" o ON o.id=q.organization_id WHERE o.active AND o.id=$1", owner.OrganizationID).Scan(&mode, &issuer) != nil ||
			(owner.TokenSHA256 != "" && mode != "mixed") || (owner.TokenSHA256 == "" && mode == "oidc_required" && issuer != owner.OIDCIssuer) {
			return ErrRecoveryWorkAdmission
		}
		first := !created[owner.PrincipalID]
		if first {
			if _, err := tx.Exec(ctx, "INSERT INTO "+q("principals")+"(id,name) VALUES($1,$2)", owner.PrincipalID, owner.DisplayName); err != nil {
				return ErrRecoveryWorkAdmission
			}
			created[owner.PrincipalID] = true
		}
		if _, err := tx.Exec(ctx, "INSERT INTO "+q("memberships")+"(organization_id,principal_id,role) VALUES($1,$2,'owner')", owner.OrganizationID, owner.PrincipalID); err != nil {
			return ErrRecoveryWorkAdmission
		}
		if owner.TokenSHA256 != "" {
			if _, err := tx.Exec(ctx, "INSERT INTO "+q("access_tokens")+"(token_hash,principal_id,expires_at) VALUES($1,$2,pg_catalog.clock_timestamp()+interval '24 hours')", owner.TokenSHA256, owner.PrincipalID); err != nil {
				return ErrRecoveryWorkAdmission
			}
		} else if first {
			if _, err := tx.Exec(ctx, "INSERT INTO "+q("identity_mappings")+"(issuer,subject,principal_id) SELECT $1,$2,$3 WHERE NOT EXISTS(SELECT 1 FROM "+
				q("identity_mappings")+" WHERE issuer=$1 AND subject=$2)", owner.OIDCIssuer, owner.OIDCSubject, owner.PrincipalID); err != nil {
				return ErrRecoveryWorkAdmission
			}
		}
	}
	if _, err := tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
		return ErrRecoveryWorkAdmission
	}
	for _, table := range tables {
		if _, err := tx.Exec(ctx, "ALTER TABLE "+q(table)+" ENABLE TRIGGER recovery_runtime_write"); err != nil {
			return ErrRecoveryWorkAdmission
		}
	}
	_, err := tx.Exec(ctx, "SET LOCAL search_path TO pg_catalog,pg_temp")
	if err != nil {
		return ErrRecoveryWorkAdmission
	}
	return nil
}

func (s *Store) installRecoveryOIDCRebindingsTx(ctx context.Context, tx pgx.Tx, request RecoveryWorkAdmissionRequest) error {
	q := func(name string) string { return pgx.Identifier{s.runtimeSchema, name}.Sanitize() }
	seen := make(map[string]bool)
	for _, owner := range request.Owners {
		if owner.OIDCIssuer == "" || seen[owner.PrincipalID] {
			continue
		}
		seen[owner.PrincipalID] = true
		if _, err := tx.Exec(ctx, "INSERT INTO "+q("recovery_oidc_rebindings")+`(issuer,subject,generation,organization_id,previous_principal_id,principal_id,activation_id)
 SELECT i.issuer,i.subject,COALESCE(r.generation,0)+1,$4,COALESCE(r.principal_id,i.principal_id),$3,$5
 FROM `+q("identity_mappings")+` i LEFT JOIN LATERAL
 (SELECT generation,principal_id FROM `+q("recovery_oidc_rebindings")+` WHERE issuer=i.issuer AND subject=i.subject ORDER BY generation DESC LIMIT 1) r ON true
 WHERE i.issuer=$1 AND i.subject=$2 AND i.principal_id<>$3`, owner.OIDCIssuer, owner.OIDCSubject,
			owner.PrincipalID, owner.OrganizationID, request.ActivationID); err != nil {
			return ErrRecoveryWorkAdmission
		}
	}
	return nil
}

func (s *Store) createRecoveryRuntimeTx(ctx context.Context, tx pgx.Tx, verifier string) (string, uint32, error) {
	p := s.recoveryProvision.intent
	if s.runtimeSchema != "public" || !validRecoveryProvision(p) || p.WALTimeline == "" || !recoveryWorkVerifier.MatchString(verifier) {
		return "", 0, ErrRecoveryWorkAdmission
	}
	name := recoveryRuntimeRoleName(p.OperationID)
	limit := strconv.Itoa(recoveryRuntimeConnectionLimit)
	role, db := pgx.Identifier{name}.Sanitize(), pgx.Identifier{p.Database}.Sanitize()
	if _, err := tx.Exec(ctx, "CREATE ROLE "+role+" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS CONNECTION LIMIT "+limit+" PASSWORD '"+verifier+"'"); err != nil {
		return "", 0, ErrRecoveryWorkAdmission
	}
	if _, err := tx.Exec(ctx, "GRANT CONNECT ON DATABASE "+db+" TO "+role+"; ALTER DATABASE "+db+" CONNECTION LIMIT "+limit+"; GRANT USAGE ON SCHEMA public TO "+role); err != nil {
		return "", 0, ErrRecoveryWorkAdmission
	}
	inventory, err := recoverySnapshotInventory(ctx, tx, s.runtimeSchema, false)
	if err != nil || digestValue(inventory) != recoverySnapshotInventorySHA256 {
		return "", 0, ErrRecoveryWorkAdmission
	}
	var tables []string
	for _, table := range inventory.Tables {
		if table.Exported && table.Name != "recovery_oidc_rebindings" {
			tables = append(tables, pgx.Identifier{s.runtimeSchema, table.Name}.Sanitize())
		}
	}
	if _, err = tx.Exec(ctx, "GRANT SELECT,INSERT,UPDATE,DELETE ON TABLE "+strings.Join(tables, ",")+" TO "+role+
		"; GRANT SELECT ON TABLE public.recovery_import_receipts,public.recovery_historical_rows,public.schema_migrations TO "+role+
		"; GRANT USAGE,SELECT ON SEQUENCE public.credential_origin_sequence,public.events_sequence_seq TO "+role); err != nil {
		return "", 0, ErrRecoveryWorkAdmission
	}
	var oid uint32
	var canCreate bool
	if tx.QueryRow(ctx, "SELECT oid,pg_catalog.has_schema_privilege(oid,'public','CREATE') FROM pg_catalog.pg_roles WHERE rolname=$1", name).Scan(&oid, &canCreate) != nil || oid == 0 || canCreate {
		return "", 0, ErrRecoveryWorkAdmission
	}
	return name, oid, nil
}
