// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"time"

	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/recoveryprocess"
)

type recoveryProcessFile struct {
	Socket string                 `json:"docker_socket"`
	Intent recoveryprocess.Intent `json:"intent"`
}

func runRecoveryProcessHost(ctx context.Context, args []string, out io.Writer) error {
	usage := errors.New("usage: platform recovery container-stop-plan|container-stop-inspect|container-stop-apply [explicit private file options]")
	if len(args) == 0 {
		return usage
	}
	f := flag.NewFlagSet("container-stop", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var socket, project, bundle, path, expected string
	var confirmed bool
	switch args[0] {
	case "container-stop-plan":
		f.StringVar(&socket, "docker-socket", "", "explicit local Docker Unix socket")
		f.StringVar(&project, "project", "", "exact Compose project, reviewed independently of archive contents")
		f.StringVar(&bundle, "backup-dir", "", "transported backup directory excluded from intent output")
		f.StringVar(&path, "out", "", "new private shutdown intent outside backup directory")
	case "container-stop-inspect", "container-stop-apply":
		f.StringVar(&path, "intent-file", "", "private saved shutdown plan")
		if args[0] == "container-stop-apply" {
			f.StringVar(&expected, "expected-project", "", "exact reviewed project name")
			f.BoolVar(&confirmed, "confirm-stop-project", false, "stop every bound container including PostgreSQL and disable automatic restart")
		}
	default:
		return usage
	}
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || path == "" {
		return usage
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var saved recoveryProcessFile
	if args[0] == "container-stop-plan" {
		if socket == "" || project == "" || bundle == "" {
			return usage
		}
		if recoveryarchive.RequireHostPathOutside(bundle, socket) != nil || recoveryarchive.RequireHostPathOutside(bundle, path) != nil {
			return errors.New("shutdown authority must be outside the transported backup")
		}
		saved.Socket = socket
	} else {
		if readRecoveryFenceJSON(path, &saved) != nil {
			return errors.New("invalid private container shutdown intent")
		}
		if args[0] == "container-stop-apply" && (!confirmed || expected == "" || expected != saved.Intent.Project) {
			return usage
		}
	}
	c, err := recoveryprocess.New(saved.Socket)
	if err != nil {
		return err
	}
	defer c.Close()
	if args[0] == "container-stop-plan" {
		saved.Intent, err = c.Plan(ctx, project)
		if err != nil {
			return err
		}
		if err = recoveryarchive.WriteHostJSONOutside(bundle, path, saved); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(struct {
			Scope  string                 `json:"scope"`
			Intent recoveryprocess.Intent `json:"intent"`
		}{"local_compose_project_processes_only", saved.Intent})
	}
	var observation recoveryprocess.Observation
	if args[0] == "container-stop-apply" {
		// A durable one-use attempt marker prevents a second apply from silently
		// becoming a retry after lost output or an uncertain Docker response.
		// Inspection remains available; continuation requires a new reviewed plan.
		if _, err = c.Inspect(ctx, saved.Intent); err != nil {
			return err
		}
		if err = recoveryarchive.WriteHostJSON(path+".apply", struct {
			OperationID string    `json:"operation_id"`
			StartedAt   time.Time `json:"started_at"`
		}{saved.Intent.OperationID, time.Now().UTC()}); err != nil {
			return errors.New("shutdown attempt already recorded or could not be saved; inspect before preparing a new plan")
		}
		observation, err = c.Stop(ctx, saved.Intent)
	} else {
		observation, err = c.Inspect(ctx, saved.Intent)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(observation)
}
