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
	"time"

	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/recoveryprocess"
	"agentplatform.local/platform/internal/storage"
	"github.com/jackc/pgx/v5"
)

const sourceRetirementSealSuffix = ".retirement-seal"

func planSourceRetirementDestination(ctx context.Context, path string) (storage.RecoveryRetirementDestination, error) {
	admin, err := openRecoveryRetirementAdminFile(ctx, path)
	if err != nil {
		return storage.RecoveryRetirementDestination{}, err
	}
	defer closeRecoveryRetirementAdmin(admin)
	return storage.PlanRecoveryRetirementDestination(ctx, admin)
}

func openRecoveryRetirementAdminFile(ctx context.Context, path string) (*pgx.Conn, error) {
	var cfg recoveryDatabaseFile
	if readRecoveryFenceJSON(path, &cfg) != nil {
		return nil, errors.New("private retirement maintenance configuration required")
	}
	return openRecoveryRetirementAdmin(ctx, cfg.DatabaseURL)
}

func openRecoveryRetirementAdmin(ctx context.Context, raw string) (*pgx.Conn, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Query().Has("search_path") {
		return nil, errors.New("retirement maintenance requires a connection without search_path")
	}
	dsn, err := explicitRecoveryDSN(raw)
	if err != nil {
		return nil, err
	}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, errors.New("cannot connect to the retirement maintenance database")
	}
	return admin, nil
}

func closeRecoveryRetirementAdmin(admin *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = admin.Close(ctx)
}

func openCapturedSourceMaintenance(ctx context.Context, c *recoveryprocess.Client, saved recoveryProcessFile) (*pgx.Conn, error) {
	// Resolve from the exact still-running source container. The URL must
	// never enter any intent, output or diagnostic.
	raw, err := c.EvidenceDatabaseURL(ctx, saved.Intent, *saved.Binding)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid source maintenance endpoint")
	}
	u.Path, u.RawPath = "/postgres", ""
	u.RawQuery = "sslmode=disable"
	return openRecoveryRetirementAdmin(ctx, u.String())
}

func openRetirementCapture(ctx context.Context, saved recoveryProcessFile, path string) (*recoveryarchive.VerifiedCaptureLink, *recoveryarchive.VerifiedArchive, error) {
	if err := validateRetirementPaths(saved, path); err != nil {
		return nil, nil, err
	}
	link, err := loadSourceCaptureLink(saved, path)
	if err != nil || link.Link().Version != 2 {
		return nil, nil, errors.New("retirement requires a drained V2 capture")
	}
	archive, err := openSourceCaptureArchive(ctx, saved, link)
	if err != nil {
		return nil, nil, errors.New("retirement requires the complete authenticated source capture")
	}
	return link, archive, nil
}

func validateRetirementPaths(saved recoveryProcessFile, path string) error {
	if saved.RetireTo == nil {
		return storage.ErrRecoverySourceRetirement
	}
	for _, suffix := range []string{".retirement-intent", ".retirement", sourceRetirementSealSuffix} {
		if validateSourceStopPath(saved, path+suffix) != nil {
			return errors.New("retirement requires private files outside both archives")
		}
	}
	return nil
}

func sourceRetirementBinding(p storage.RecoverySourceRetirementIntent, o storage.RecoverySourceRetirementObservation) recoveryarchive.SourceRetirementBinding {
	return recoveryarchive.SourceRetirementBinding{
		RestoreID: p.Destination.RestoreID, IntentSHA256: captureHash(p), AdmissionReceiptSHA256: o.ReceiptSHA256, CutProofSHA256: o.CutProofSHA256,
	}
}

func readSourceRetirementSeal(saved recoveryProcessFile, path string, link *recoveryarchive.VerifiedCaptureLink, p storage.RecoverySourceRetirementIntent, o storage.RecoverySourceRetirementObservation) (*recoveryarchive.VerifiedSourceRetirementSeal, error) {
	file := path + sourceRetirementSealSuffix
	if _, err := os.Lstat(file); errors.Is(err, os.ErrNotExist) {
		// Older, unsigned retirement observations may accompany held imports.
		return nil, nil
	} else if err != nil {
		return nil, storage.ErrRecoverySourceRetirement
	}
	seal, err := recoveryarchive.VerifyHostSourceRetirementSeal(file, saved.Capture.Keys, link)
	if err != nil || p.Version != 2 || seal.Binding() != sourceRetirementBinding(p, o) {
		return nil, errors.New("retirement seal differs from the saved source and destination")
	}
	return seal, nil
}

func readSavedSourceRetirement(saved recoveryProcessFile, path string, link *recoveryarchive.VerifiedCaptureLink) (storage.RecoverySourceRetirementIntent, storage.RecoverySourceRetirementObservation, *recoveryarchive.VerifiedSourceRetirementSeal, error) {
	var p storage.RecoverySourceRetirementIntent
	var o storage.RecoverySourceRetirementObservation
	if err := validateRetirementPaths(saved, path); err != nil {
		return p, o, nil, err
	}
	if readRecoveryFenceJSON(path+".retirement-intent", &p) != nil || !p.MatchesCapture(link, *saved.RetireTo) ||
		readRecoveryFenceJSON(path+".retirement", &o) != nil || !o.ConfirmsRetirement(p) {
		return p, o, nil, storage.ErrRecoverySourceRetirement
	}
	seal, err := readSourceRetirementSeal(saved, path, link, p, o)
	return p, o, seal, err
}

func retireCapturedSource(ctx context.Context, c *recoveryprocess.Client, saved recoveryProcessFile, path string, source *storage.Store) (recoveryprocess.Observation, error) {
	link, archive, err := openRetirementCapture(ctx, saved, path)
	if err != nil {
		return recoveryprocess.Observation{}, err
	}
	defer archive.Close()
	admin, err := openCapturedSourceMaintenance(ctx, c, saved)
	if err != nil {
		return recoveryprocess.Observation{}, err
	}
	defer closeRecoveryRetirementAdmin(admin)
	cut, err := storage.PrepareRecoverySourceCut(ctx, admin, source, archive, link, *saved.RetireTo, storage.DefaultRecoverySnapshotLimits())
	if err != nil {
		return recoveryprocess.Observation{}, err
	}
	defer cut.Close()
	if err = archive.Close(); err != nil {
		return recoveryprocess.Observation{}, err
	}
	// Only the independent read-only backend keeps its transaction and locks.
	source.Close()
	intent := cut.Intent()
	if err = recoveryarchive.WriteHostJSONOutside(saved.Capture.Directory, path+".retirement-intent", intent); err != nil {
		return recoveryprocess.Observation{}, err
	}
	// Wait only by observing. The database's own read-only healthcheck may
	// still be closing its session. Apply is attempted at most once.
	wait, stop := context.WithTimeout(ctx, 8*time.Second)
	defer stop()
	for {
		o, e := storage.InspectRecoverySourceRetirement(wait, admin, intent)
		if e != nil || o.State != "source_admission_open" || o.PreparedTransactions != 0 {
			return recoveryprocess.Observation{}, storage.ErrRecoverySourceRetirement
		}
		if o.Sessions == 1 {
			break
		}
		if o.Sessions == 0 {
			return recoveryprocess.Observation{}, storage.ErrRecoverySourceCut
		}
		select {
		case <-wait.Done():
			return recoveryprocess.Observation{}, storage.ErrRecoverySourceRetirement
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err = checkSourceRetirementDestination(ctx, saved); err != nil {
		return recoveryprocess.Observation{}, err
	}
	if _, err = c.ObserveSourceCapture(ctx, saved.Intent, *saved.Binding); err != nil {
		return recoveryprocess.Observation{}, err
	}
	if _, err = cut.Retire(ctx, admin); errors.Is(err, storage.ErrRecoverySourceRetirementUnknown) {
		// Reopen the exact bound maintenance endpoint only to observe the
		// outcome. Confirm requires both a committed ban and the same cut.
		fresh, openErr := openCapturedSourceMaintenance(ctx, c, saved)
		if openErr != nil {
			return recoveryprocess.Observation{}, err
		}
		defer closeRecoveryRetirementAdmin(fresh)
		o, inspectErr := storage.InspectRecoverySourceRetirement(ctx, fresh, intent)
		if inspectErr != nil || o.SourceConnections || o.ReceiptSHA256 == "" {
			return recoveryprocess.Observation{}, err
		}
		admin = fresh
	} else if err != nil {
		return recoveryprocess.Observation{}, err
	}
	if err = cut.Confirm(ctx, admin); err != nil {
		return recoveryprocess.Observation{}, err
	}
	cut.Close()
	wait, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	for {
		o, e := storage.InspectRecoverySourceRetirement(wait, admin, intent)
		if e != nil || o.CutProofSHA256 == "" || o.PreparedTransactions != 0 {
			return recoveryprocess.Observation{}, storage.ErrRecoverySourceCut
		}
		if o.Sessions == 0 {
			return stopRetiredSource(ctx, c, saved, path, link, intent, o)
		}
		select {
		case <-wait.Done():
			return recoveryprocess.Observation{}, storage.ErrRecoverySourceRetirement
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func checkSourceRetirementDestination(ctx context.Context, saved recoveryProcessFile) error {
	if validateSourceStopPath(saved, saved.RetireConfig) != nil {
		return errors.New("retirement destination configuration must be private and outside both archives")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	destination, err := openRecoveryRetirementAdminFile(ctx, saved.RetireConfig)
	if err != nil {
		return err
	}
	defer closeRecoveryRetirementAdmin(destination)
	if err = storage.CheckRecoveryRetirementDestination(ctx, destination, *saved.RetireTo); err != nil {
		return errors.New("the reserved retirement destination is unavailable or changed; source admission was not changed")
	}
	return nil
}

// Continue only after a fresh inspection sees the exact committed ban. A
// failed or unknown retirement mutation is never retried by this command.
func finishCapturedSourceRetirement(ctx context.Context, c *recoveryprocess.Client, saved recoveryProcessFile, path string) (recoveryprocess.Observation, error) {
	link, archive, err := openRetirementCapture(ctx, saved, path)
	if err != nil {
		return recoveryprocess.Observation{}, err
	}
	if err = archive.Close(); err != nil {
		return recoveryprocess.Observation{}, err
	}
	var intent storage.RecoverySourceRetirementIntent
	if readRecoveryFenceJSON(path+".retirement-intent", &intent) != nil || !intent.MatchesCapture(link, *saved.RetireTo) {
		return recoveryprocess.Observation{}, storage.ErrRecoverySourceRetirement
	}
	admin, err := openCapturedSourceMaintenance(ctx, c, saved)
	if err != nil {
		return recoveryprocess.Observation{}, err
	}
	defer closeRecoveryRetirementAdmin(admin)
	observation, err := storage.InspectRecoverySourceRetirement(ctx, admin, intent)
	if err != nil {
		return recoveryprocess.Observation{}, err
	}
	return stopRetiredSource(ctx, c, saved, path, link, intent, observation)
}

func stopRetiredSource(ctx context.Context, c *recoveryprocess.Client, saved recoveryProcessFile, path string, link *recoveryarchive.VerifiedCaptureLink, intent storage.RecoverySourceRetirementIntent, o storage.RecoverySourceRetirementObservation) (recoveryprocess.Observation, error) {
	if saved.RetireTo == nil || !intent.MatchesCapture(link, *saved.RetireTo) || !o.ConfirmsRetirement(intent) {
		return recoveryprocess.Observation{}, storage.ErrRecoverySourceRetirement
	}
	report := path + ".retirement"
	if _, err := os.Lstat(report); errors.Is(err, os.ErrNotExist) {
		if err = recoveryarchive.WriteHostJSONOutside(saved.Capture.Directory, report, o); err != nil {
			return recoveryprocess.Observation{}, err
		}
	} else {
		var old storage.RecoverySourceRetirementObservation
		if err != nil || readRecoveryFenceJSON(report, &old) != nil || !old.ConfirmsRetirement(intent) || old.ReceiptSHA256 != o.ReceiptSHA256 || old.CutProofSHA256 != o.CutProofSHA256 {
			return recoveryprocess.Observation{}, errors.New("existing retirement observation differs; preserve it and inspect the source")
		}
	}
	seal, err := readSourceRetirementSeal(saved, path, link, intent, o)
	if err != nil {
		return recoveryprocess.Observation{}, err
	}
	if intent.Version == 2 && seal == nil {
		// Persist the signed live-catalog observation before the database stop.
		// If the stop response is lost, the final receipt can be reconstructed
		// by observing the same stopped generation without starting it.
		if err = recoveryarchive.WriteHostSourceRetirementSeal(path+sourceRetirementSealSuffix, saved.Capture.Keys, link, sourceRetirementBinding(intent, o)); err != nil {
			return recoveryprocess.Observation{}, err
		}
	}
	return c.StopDatabaseAfterEvidence(ctx, saved.Intent)
}

func runSourceRetirementInspectHost(ctx context.Context, args []string, out io.Writer) error {
	f := flag.NewFlagSet("source-retirement-inspect", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	path := f.String("retirement-intent-file", "", "private saved retirement intent")
	config := f.String("maintenance-config", "", "private maintenance connection to postgres on the source")
	if f.Parse(args) != nil || f.NArg() != 0 || *path == "" || *config == "" {
		return errors.New("usage: platform recovery source-retirement-inspect --retirement-intent-file PRIVATE_INTENT --maintenance-config PRIVATE_SOURCE_MAINTENANCE")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var intent storage.RecoverySourceRetirementIntent
	if readRecoveryFenceJSON(*path, &intent) != nil {
		return errors.New("private retirement intent required")
	}
	admin, err := openRecoveryRetirementAdminFile(ctx, *config)
	if err != nil {
		return err
	}
	defer closeRecoveryRetirementAdmin(admin)
	o, err := storage.InspectRecoverySourceRetirement(ctx, admin, intent)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(o)
}
