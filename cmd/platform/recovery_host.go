// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"agentplatform.local/platform/internal/recoveryaccess"
	"agentplatform.local/platform/internal/storage"
)

var recoveryUsage = errors.New("usage: platform recovery access-init|access-inspect|access-replace|access-resolve|issue-read-token|list-tasks|read-result --authority-dir ABSOLUTE_PRIVATE_DIRECTORY [command options]")

// Explicit private config only. Never adopt DATABASE_URL or another installation's
// saved config implicitly. This path does not migrate, bootstrap or start workers.
type recoveryHostTarget struct {
	DatabaseURL          string `json:"database_url"`
	TargetInstallationID string `json:"target_installation_id"`
	RestoreID            string `json:"restore_id"`
	BackupID             string `json:"backup_id"`
	SourceID             string `json:"source_id"`
	CutUTC               string `json:"cut_utc"`
	SchemaSHA256         string `json:"schema_sha256"`
	SnapshotSHA256       string `json:"snapshot_sha256"`
	ReceiptSHA256        string `json:"receipt_sha256,omitempty"`
}
type recoveryHostScopes struct {
	Scopes []struct {
		Subject        string `json:"subject"`
		OrganizationID string `json:"organization_id"`
	} `json:"scopes"`
}

func (c recoveryHostTarget) spec() storage.RecoveryTargetSpec {
	return storage.RecoveryTargetSpec{TargetInstallationID: c.TargetInstallationID, RestoreID: c.RestoreID, BackupID: c.BackupID, SourceID: c.SourceID, CutUTC: c.CutUTC, SchemaSHA256: c.SchemaSHA256}
}
func (c recoveryHostTarget) binding() recoveryaccess.Binding {
	return recoveryaccess.Binding{TargetID: c.TargetInstallationID, RestoreID: c.RestoreID, SnapshotSHA256: c.SnapshotSHA256}
}

func runRecoveryHost(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return recoveryUsage
	}
	switch args[0] {
	case "backup-keygen", "backup-create", "archive-verify", "target-prepare", "target-inspect", "archive-import", "import-inspect":
		return runRecoveryArchiveHost(ctx, args, out)
	}
	f := flag.NewFlagSet("recovery", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	path := f.String("authority-dir", "", "existing private authority directory; new for access-init")
	var targetFile, scopeFile, reference, operation, subject, org, tokenFile, tokenOut, after, candidate string
	var confirmed bool
	var epoch uint64
	var lifetime time.Duration
	var limit int
	registerTarget := func() { f.StringVar(&targetFile, "target-config", "", "explicit private restored-target JSON file") }
	registerScope := func() {
		f.StringVar(&scopeFile, "scope-file", "", "private current subject/organization scope JSON")
		f.StringVar(&reference, "operator-reference", "", "current authority evidence reference")
		f.BoolVar(&confirmed, "confirm-current-authority", false, "attest that scopes were verified independently of the backup")
	}
	switch args[0] {
	case "access-init":
		registerTarget()
		registerScope()
	case "access-inspect":
	case "access-replace":
		registerScope()
		f.Uint64Var(&epoch, "expected-epoch", 0, "exact inspected epoch")
	case "access-resolve":
		f.StringVar(&operation, "operation-id", "", "exact inspected pending operation")
	case "issue-read-token":
		f.StringVar(&subject, "subject", "", "current scope recipient")
		f.StringVar(&org, "organization-id", "", "organization")
		f.DurationVar(&lifetime, "lifetime", 5*time.Minute, "maximum15m")
		f.StringVar(&tokenOut, "out", "", "new private token file outside authority directory")
	case "list-tasks", "read-result":
		registerTarget()
		f.StringVar(&org, "organization-id", "", "organization")
		f.StringVar(&tokenFile, "token-file", "", "private read capability file")
		if args[0] == "list-tasks" {
			f.StringVar(&after, "after-id", "", "page cursor")
			f.IntVar(&limit, "limit", 20, "1..50")
		} else {
			f.StringVar(&candidate, "candidate-id", "", "exact saved result candidate")
		}
	default:
		return recoveryUsage
	}
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || *path == "" {
		return recoveryUsage
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	emit := func(value any) error { return json.NewEncoder(out).Encode(value) }
	loadScopes := func() ([]recoveryaccess.Scope, error) {
		if !confirmed || reference == "" || scopeFile == "" {
			return nil, recoveryUsage
		}
		var config recoveryHostScopes
		if readPrivateJSON(scopeFile, &config) != nil || config.Scopes == nil {
			return nil, errors.New("invalid private current-scope file")
		}
		result := make([]recoveryaccess.Scope, 0, len(config.Scopes))
		for _, scope := range config.Scopes {
			result = append(result, recoveryaccess.Scope{Subject: scope.Subject, OrganizationID: scope.OrganizationID})
		}
		return result, nil
	}
	switch args[0] {
	case "access-inspect":
		value, err := recoveryaccess.InspectHost(ctx, *path)
		if err != nil {
			return err
		}
		return emit(value)
	case "access-replace":
		scopes, err := loadScopes()
		if err != nil {
			return err
		}
		if epoch == 0 {
			return recoveryUsage
		}
		value, err := recoveryaccess.ReplaceHost(ctx, *path, epoch, scopes, reference)
		if err != nil {
			return err
		}
		return emit(value)
	case "access-resolve":
		if operation == "" {
			return recoveryUsage
		}
		value, err := recoveryaccess.ResolveHost(ctx, *path, operation)
		if err != nil {
			return err
		}
		return emit(value)
	case "issue-read-token":
		token, err := recoveryaccess.IssueHost(ctx, *path, subject, org, lifetime)
		if err != nil {
			return err
		}
		if err = recoveryaccess.SaveHostTokenOutside(*path, tokenOut, token); err != nil {
			return err
		}
		return emit(struct {
			Status string `json:"status"`
		}{"private_token_file_created"})
	}
	// Only init and the two reader operations reach a database. All require the
	// exact held import tuple from a private explicit file, independent of policy.
	var config recoveryHostTarget
	if targetFile == "" || readPrivateJSON(targetFile, &config) != nil || config.DatabaseURL == "" {
		return errors.New("explicit private recovery target configuration required")
	}
	var scopes []recoveryaccess.Scope
	if args[0] == "access-init" {
		var err error
		scopes, err = loadScopes()
		if err != nil {
			return err
		}
	}
	dsn, err := explicitRecoveryDSN(config.DatabaseURL)
	if err != nil {
		return err
	}
	s, err := storage.Open(ctx, dsn)
	if err != nil {
		return errors.New("cannot open explicit recovery target")
	}
	defer s.Close()
	if args[0] == "access-init" {
		inspection, err := s.InspectRecoveryImport(ctx, config.spec(), config.SnapshotSHA256)
		if err != nil || inspection.State != "committed_held" {
			return errors.New("exact committed held recovery target required")
		}
		value, err := recoveryaccess.InitHost(ctx, *path, config.binding(), scopes, reference)
		if err != nil {
			return err
		}
		return emit(value)
	}
	token, err := recoveryaccess.LoadHostToken(tokenFile)
	if err != nil {
		return err
	}
	var result any
	err = recoveryaccess.WithHostAuthority(ctx, *path, func(ctx context.Context, a *recoveryaccess.Authority) error {
		if !a.BoundTo(config.binding()) {
			return recoveryaccess.ErrDenied
		}
		reader, e := storage.NewRecoveryReader(s, a)
		if e != nil {
			return e
		}
		if args[0] == "list-tasks" {
			value, e := reader.ListTasks(ctx, token, org, after, limit)
			if e == nil {
				result = value
			}
			return e
		}
		value, e := reader.Result(ctx, token, org, candidate)
		if e == nil {
			result = value
		}
		return e
	})
	if err != nil {
		return err
	}
	return emit(result)
}

// Refuse pgx's ambient PG* configuration and incomplete URI selectors. Only a
// single explicit endpoint is accepted; no service/passfile fallback is used.
func explicitRecoveryDSN(raw string) (string, error) {
	invalid := errors.New("recovery requires a complete PostgreSQL URI and no ambient PG settings")
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "PG") && value != "" {
			return "", invalid
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Scheme != "postgres" && u.Scheme != "postgresql" || u.User == nil || u.User.Username() == "" || u.Hostname() == "" || u.Port() == "" || strings.Contains(u.Host, ",") || u.Fragment != "" || u.Path == "" || u.Path == "/" || strings.Contains(strings.TrimPrefix(u.Path, "/"), "/") {
		return "", invalid
	}
	password, present := u.User.Password()
	if !present || password == "" {
		return "", invalid
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", invalid
	}
	for key, values := range q {
		if len(values) != 1 || key != "sslmode" && key != "sslrootcert" && key != "search_path" {
			return "", invalid
		}
	}
	if q.Get("sslmode") != "disable" && q.Get("sslmode") != "verify-full" {
		return "", invalid
	}
	if q.Get("sslmode") == "verify-full" && !filepath.IsAbs(q.Get("sslrootcert")) {
		return "", invalid
	}
	if schema := q.Get("search_path"); schema != "" && !regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`).MatchString(schema) {
		return "", invalid
	}
	// Explicitly disable user-home client certificate and password-file defaults.
	q.Set("sslcert", "")
	q.Set("sslkey", "")
	q.Set("sslpassword", "")
	q.Set("passfile", "")
	if q.Get("sslmode") == "disable" {
		q.Set("sslrootcert", "")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
