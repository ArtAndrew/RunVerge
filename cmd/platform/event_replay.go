// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"strconv"

	"agentplatform.local/platform/internal/storage"
)

// Host/database authority is required. This never prunes durable history and
// exposes no tenant-facing mutation or automatic dispatch after restoration.
func runAdvanceEventReplay(ctx context.Context, s *storage.Store, args []string) error {
	f := flag.NewFlagSet("advance-event-replay", flag.ContinueOnError)
	org := f.String("organization", "", "organization ID")
	task := f.String("task", "", "task ID")
	after := f.String("after", "", "canonical decimal replay boundary")
	reason := f.String("reason", "", "audit reason")
	if err := f.Parse(args); err != nil {
		return err
	}
	n, err := strconv.ParseInt(*after, 10, 64)
	if f.NArg() != 0 || *org == "" || *task == "" || *reason == "" || err != nil || n < 0 || strconv.FormatInt(n, 10) != *after {
		return errors.New("advance-event-replay requires organization, task, canonical nonnegative after and audit reason")
	}
	v, err := s.AdvanceEventReplayWindow(ctx, *org, *task, n, *reason)
	if err != nil {
		return errors.New("event replay boundary rejected; verify task, monotonic position and audit reason")
	}
	return json.NewEncoder(os.Stdout).Encode(v)
}
