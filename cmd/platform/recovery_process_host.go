// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/recoveryprocess"
	"agentplatform.local/platform/internal/storage"
)

type recoveryProcessFile struct {
	Socket  string                         `json:"docker_socket"`
	Intent  recoveryprocess.Intent         `json:"intent"`
	Binding *recoveryprocess.SourceBinding `json:"source_binding,omitempty"`
	Capture *recoverySourceCapture         `json:"evidence_capture,omitempty"`
}

func runRecoveryProcessHost(ctx context.Context, args []string, out io.Writer) error {
	usage := errors.New("usage: platform recovery container-stop-plan|container-stop-inspect|container-stop-apply|source-stop-plan|source-stop-inspect|source-stop-apply [explicit private file options]")
	if len(args) == 0 {
		return usage
	}
	f := flag.NewFlagSet("container-stop", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var socket, project, bundle, path, expected string
	var confirmed bool
	bound := strings.HasPrefix(args[0], "source-stop-")
	operation := strings.Replace(args[0], "source-stop-", "container-stop-", 1)
	var keys, stage, receipt, image, evidence string
	switch operation {
	case "container-stop-plan":
		f.StringVar(&socket, "docker-socket", "", "explicit local Docker Unix socket")
		f.StringVar(&project, "project", "", "exact Compose project, reviewed independently of archive contents")
		f.StringVar(&bundle, "backup-dir", "", "transported backup directory excluded from intent output")
		f.StringVar(&path, "out", "", "new private shutdown intent outside backup directory")
		if bound {
			f.StringVar(&keys, "keys-file", "", "independently trusted archive keys")
			f.StringVar(&stage, "staging-parent", "", "private archive staging directory")
			f.StringVar(&receipt, "expected-receipt-sha256", "", "independently recorded receipt digest")
			f.StringVar(&image, "expected-runtime-image", "", "independently trusted immutable runtime image ID")
			f.StringVar(&evidence, "evidence-dir", "", "new encrypted post-backup evidence directory; native Linux host only")
		}
	case "container-stop-inspect", "container-stop-apply":
		f.StringVar(&path, "intent-file", "", "private saved shutdown plan")
		if operation == "container-stop-apply" {
			f.StringVar(&expected, "expected-project", "", "exact reviewed project name")
			f.BoolVar(&confirmed, "confirm-stop-project", false, "stop every bound container including PostgreSQL and disable automatic restart")
		}
	default:
		return usage
	}
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || path == "" {
		return usage
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
	defer cancel()
	var saved recoveryProcessFile
	var source, snapshot string
	if operation == "container-stop-plan" {
		if socket == "" || project == "" || bundle == "" {
			return usage
		}
		if recoveryarchive.RequireHostPathOutside(bundle, socket) != nil || recoveryarchive.RequireHostPathOutside(bundle, path) != nil {
			return errors.New("shutdown authority must be outside the transported backup")
		}
		saved.Socket = socket
		if bound {
			if keys == "" || stage == "" || receipt == "" || image == "" {
				return usage
			}
			archive, err := recoveryarchive.VerifyHostBackup(ctx, bundle, keys, receipt, stage)
			if err != nil {
				return err
			}
			defer archive.Close()
			report, err := storage.ValidateRecoverySnapshot(ctx, archive, storage.DefaultRecoverySnapshotLimits())
			if err != nil {
				return errors.New("selected snapshot framing invalid")
			}
			m, manifest := report.Metadata, archive.Manifest()
			if manifest.Metadata != (recoveryarchive.Metadata{BackupID: m.BackupID, SourceID: m.SourceID, CutUTC: m.CutUTC, SchemaSHA256: m.SchemaSHA256}) || manifest.PlaintextSHA256 != report.SHA256 || manifest.PlaintextBytes != report.WireBytes || m.SchemaSHA256 != storage.EmbeddedSchemaSHA256() {
				return errors.New("source binding requires a matching authenticated snapshot")
			}
			if err = archive.Close(); err != nil {
				return err
			}
			source, snapshot = m.SourceID, report.SHA256
			if evidence != "" {
				saved.Capture = &recoverySourceCapture{Directory: evidence, Keys: keys, Stage: stage, SelectedDirectory: bundle, SelectedReceipt: receipt, SelectedCut: m.CutUTC}
				if err = validateSourceCapturePaths(bundle, path, *saved.Capture); err != nil {
					return err
				}
			}
		}
	} else {
		if readRecoveryFenceJSON(path, &saved) != nil {
			return errors.New("invalid private container shutdown intent")
		}
		if bound != (saved.Binding != nil) {
			return errors.New("use the command family matching the saved shutdown plan")
		}
		if saved.Capture != nil && !bound {
			return usage
		}
		if operation == "container-stop-apply" && (!confirmed || expected == "" || expected != saved.Intent.Project) {
			return usage
		}
	}
	c, err := recoveryprocess.New(saved.Socket)
	if err != nil {
		return err
	}
	defer c.Close()
	if operation == "container-stop-plan" {
		saved.Intent, err = c.Plan(ctx, project)
		if err != nil {
			if bound {
				return fmt.Errorf("%w (source:container_inventory)", err)
			}
			return err
		}
		if bound {
			binding, e := c.BindSource(ctx, saved.Intent, source, snapshot, image)
			if e != nil {
				return e
			}
			saved.Binding = &binding
		}
		if err = recoveryarchive.WriteHostJSONOutside(bundle, path, saved); err != nil {
			return err
		}
		scope := "local_compose_project_processes_only"
		if bound {
			scope = "bound_source_compose_processes_only"
		}
		return json.NewEncoder(out).Encode(struct {
			Binding *recoveryprocess.SourceBinding `json:"source_binding,omitempty"`
			Scope   string                         `json:"scope"`
			Intent  recoveryprocess.Intent         `json:"intent"`
		}{saved.Binding, scope, saved.Intent})
	}
	var observation recoveryprocess.Observation
	if operation == "container-stop-apply" {
		// A durable one-use attempt marker prevents a second apply from silently
		// becoming a retry after lost output or an uncertain Docker response.
		// Inspection remains available; continuation requires a new reviewed plan.
		if _, err = c.Inspect(ctx, saved.Intent); err != nil {
			return err
		}
		if bound {
			if err = c.RevalidateSource(ctx, saved.Intent, *saved.Binding); err != nil {
				return err
			}
		}
		var sourceStore *storage.Store
		if saved.Capture != nil {
			sourceStore, err = prepareSourceCapture(ctx, c, saved, path)
			if err != nil {
				return err
			}
			defer sourceStore.Close()
		}
		if err = recoveryarchive.WriteHostJSON(path+".apply", struct {
			OperationID string    `json:"operation_id"`
			StartedAt   time.Time `json:"started_at"`
		}{saved.Intent.OperationID, time.Now().UTC()}); err != nil {
			return errors.New("shutdown attempt already recorded or could not be saved; inspect before preparing a new plan")
		}
		if saved.Capture != nil {
			observation, err = applySourceCapture(ctx, c, saved, path, sourceStore)
		} else {
			observation, err = c.Stop(ctx, saved.Intent)
		}
	} else {
		observation, err = c.Inspect(ctx, saved.Intent)
	}
	if err != nil {
		return err
	}
	if bound {
		observation, err = c.InspectBoundSource(ctx, saved.Intent, *saved.Binding)
		if err != nil {
			return err
		}
	}
	if saved.Capture != nil {
		captured, e := inspectSourceCapture(ctx, saved, path)
		if e != nil {
			return e
		}
		return json.NewEncoder(out).Encode(struct {
			recoveryprocess.Observation
			EvidenceCaptured   bool `json:"evidence_captured"`
			EvidenceReconciled bool `json:"evidence_reconciled"`
		}{observation, captured, false})
	}
	return json.NewEncoder(out).Encode(observation)
}
