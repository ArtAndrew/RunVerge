// SPDX-License-Identifier: Elastic-2.0
// Test-only seed for a newly owned Compose database. Never shipped in runtime.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/storage"
)

type fixture struct {
	InitialSpent int64  `json:"initial_spent_microusd,string"`
	Installation string `json:"installation_id"`
	Organization string `json:"organization_id"`
	Reservation  string `json:"reservation_id"`
	Task         string `json:"task_id"`
	Invoice      string `json:"invoice_id,omitempty"`
	Correction   string `json:"correction_id,omitempty"`
}

func read(path string, out any) error {
	b, err := recoveryarchive.ReadHostPrivateFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "owned accounting source fixture failed")
		os.Exit(1)
	}
}
func run() error {
	phase := flag.String("phase", "", "prepare or append")
	config := flag.String("database-config", "", "private config for newly owned source")
	previous := flag.String("state", "", "private prepare result")
	out := flag.String("out", "", "new private fixture state")
	flag.Parse()
	fail := errors.New("invalid owned fixture")
	if os.Getenv("RUNVERGE_OWNED_RECOVERY_FIXTURE") != "1" || (*phase != "prepare" && *phase != "append") || *config == "" || *out == "" || flag.NArg() != 0 {
		return fail
	}
	var settings struct {
		DatabaseURL string `json:"database_url"`
	}
	if read(*config, &settings) != nil || settings.DatabaseURL == "" {
		return fail
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	s, err := storage.Open(ctx, settings.DatabaseURL)
	if err != nil {
		return fail
	}
	defer s.Close()
	state, err := s.InspectRecoveryTarget(ctx)
	if err != nil {
		return fail
	}
	var f fixture
	if *phase == "prepare" {
		b, err := s.Bootstrap(ctx, "Owned installed accounting source", 1000000)
		if err != nil {
			return fail
		}
		p, err := s.Authenticate(ctx, b.Token, b.OrganizationID)
		if err != nil {
			return fail
		}
		budget := int64(10000)
		raw, err := s.CreateAgent(ctx, p, domain.NewID("key"), domain.CreateAgent{Name: "Owned financial fixture", Instructions: "Synthetic fixture; no external calls", BudgetMicroUSD: &budget})
		if err != nil {
			return fail
		}
		var a domain.Agent
		if json.Unmarshal(raw, &a) != nil {
			return fail
		}
		raw, err = s.PublishAgent(ctx, p, a.ID, domain.NewID("key"), a.Version)
		if err != nil || json.Unmarshal(raw, &a) != nil {
			return fail
		}
		raw, err = s.CreateTask(ctx, p, domain.NewID("key"), domain.CreateTask{Goal: "Owned financial continuity", AgentID: a.ID, BudgetMicroUSD: 10000})
		if err != nil {
			return fail
		}
		var task domain.Task
		if json.Unmarshal(raw, &task) != nil {
			return fail
		}
		lease, err := s.ClaimFixture(ctx, "owned-financial-seed", time.Minute)
		if err != nil || lease == nil || lease.TaskID != task.ID {
			return fail
		}
		r, err := s.ReserveCost(ctx, lease, domain.ReserveCostRequest{ChargeKey: domain.NewID("charge"), Category: "compute", Provider: "owned-financial-provider", PriceVersion: "owned-test-v1", AmountMicroUSD: 37, IntentHash: storage.Hash([]byte("owned financial source"))})
		if err != nil {
			return fail
		}
		if _, err = s.MarkCostUnknown(ctx, p.OrganizationID, r.ID, "owned-financial-unknown", "no external request performed"); err != nil {
			return fail
		}
		if _, err = s.CancelTask(ctx, p, task.ID, domain.NewID("key")); err != nil {
			return fail
		}
		f = fixture{Installation: state.InstallationID, Organization: p.OrganizationID, Reservation: r.ID, Task: task.ID}
		if did, err := s.ReconcileFixtureReservation(ctx); err != nil || !did {
			return fail
		}
		var settled bool
		if s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reservations WHERE organization_id=$1 AND id=$2 AND category='fixture_legacy' AND status='settled' AND actual_microusd=amount_microusd)`, p.OrganizationID, lease.ReservationID).Scan(&settled) != nil || !settled {
			return fail
		}
		var reserved int64
		if s.Pool.QueryRow(ctx, "SELECT spent_microusd,reserved_microusd FROM organizations WHERE id=$1", p.OrganizationID).Scan(&f.InitialSpent, &reserved) != nil || reserved != 37 {
			return fail
		}
	} else {
		if *previous == "" || read(*previous, &f) != nil || f.Installation != state.InstallationID || f.Invoice != "" || f.Correction != "" {
			return fail
		}
		var matches bool
		if s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM reservations r JOIN tasks t ON t.organization_id=r.organization_id AND t.id=r.task_id WHERE r.organization_id=$1 AND r.id=$2 AND r.task_id=$3 AND r.provider='owned-financial-provider' AND r.status='unknown' AND t.status='cancelled')`, f.Organization, f.Reservation, f.Task).Scan(&matches) != nil || !matches {
			return fail
		}
		e := domain.CostEvidence{Source: "owned-financial-provider", Reference: "installed-invoice", PayloadDigest: strings.Repeat("c", 64), ActualMicroUSD: 7, Final: true, Currency: "USD"}
		first, err := s.AccountCost(ctx, f.Organization, f.Reservation, e)
		if err != nil || first.Status != "settled" {
			return fail
		}
		f.Invoice = first.EvidenceID
		e.Reference, e.ActualMicroUSD, e.Reason = "installed-correction", 3, "owned correction"
		corrected, err := s.AccountAdjustment(ctx, f.Organization, f.Reservation, domain.CostAdjustment{Evidence: e, ExpectedRevision: first.Reservation.Revision})
		if err != nil || corrected.Status != "settled" {
			return fail
		}
		f.Correction = corrected.EvidenceID
	}
	return recoveryarchive.WriteHostJSON(*out, f)
}
