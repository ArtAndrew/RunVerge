// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"time"

	"agentplatform.local/platform/internal/doctor"
	"agentplatform.local/platform/internal/domain"
)

const doctorMaxPages = 5

type doctorReader struct {
	ctx      context.Context
	base     string
	identity mcpCredentialFile
}

// Every request uses the existing shape-checked ordinary HTTP client. Pin the
// credential once, never read DATABASE_URL, never follow redirects or dispatch
// a mutation/probe. The total deadline includes all pages and final reads.
func (r doctorReader) read(group, action, id, cursor, planID string, out any) error {
	cmd, ok := configurationCLICommandFor(group, action)
	if !ok || cmd.method != "GET" {
		return errors.New("inspection_failed")
	}
	args := []string{group}
	if action != "" {
		args = append(args, action)
	}
	args = append(args, "--url", r.base, "--json")
	if id != "" {
		args = append(args, "--id", id)
	}
	if cmd.list {
		args = append(args, "--limit", "100")
	}
	if cursor != "" {
		args = append(args, "--cursor", cursor)
	}
	if planID != "" {
		args = append(args, "--plan-id", planID)
	}
	var body bytes.Buffer
	if err := runConfigurationClientWithIdentity(r.ctx, args, &body, &r.identity); err != nil {
		var rejected struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(body.Bytes(), &rejected) == nil && slices.Contains([]string{"unauthenticated", "forbidden", "not_found", "incompatible_client", "unavailable"}, rejected.Error.Code) {
			return errors.New(rejected.Error.Code)
		}
		return errors.New("inspection_failed")
	}
	if err := json.Unmarshal(body.Bytes(), out); err != nil {
		return errors.New("inspection_failed")
	}
	return nil
}

func doctorList[T any](r doctorReader, group, planID string, identity func(T) string) ([]T, error) {
	all := []T{}
	cursor := ""
	seenCursor, seenID := map[string]bool{}, map[string]bool{}
	for page := 0; page < doctorMaxPages; page++ {
		var result struct {
			Data []T    `json:"data"`
			Next string `json:"next_cursor"`
		}
		if err := r.read(group, "list", "", cursor, planID, &result); err != nil {
			return nil, err
		}
		for _, item := range result.Data {
			id := identity(item)
			if seenID[id] {
				return nil, errors.New("pagination_inconsistent")
			}
			seenID[id] = true
		}
		all = append(all, result.Data...)
		if result.Next == "" {
			return all, nil
		}
		if len(result.Data) == 0 || seenCursor[result.Next] {
			return nil, errors.New("pagination_inconsistent")
		}
		seenCursor[result.Next], cursor = true, result.Next
	}
	return nil, errors.New("inspection_limit_reached")
}

func (r doctorReader) collect(planID string) (doctor.Snapshot, error) {
	s := doctor.Snapshot{}
	var catalog struct {
		Data domain.ConfigurationCatalog `json:"data"`
	}
	if err := r.read("catalog", "", "", "", "", &catalog); err != nil {
		return s, err
	}
	s.Catalog = catalog.Data
	if !slices.Contains(s.Catalog.AvailableCommands, "inspect") {
		return s, errors.New("forbidden")
	}
	if planID != "" {
		var plan struct {
			Data domain.ConfigurationPlan `json:"data"`
		}
		if err := r.read("plans", "get", planID, "", "", &plan); err != nil {
			return s, err
		}
		s.Plan = &plan.Data
	}
	var err error
	s.Destinations, err = doctorList(r, "destinations", "", func(v domain.ConfigurationDestination) string { return fmt.Sprintf("%s:%d", v.ID, v.Version) })
	if err != nil {
		return s, err
	}
	s.Resources, err = doctorList(r, "resources", "", func(v domain.ConfigurationResource) string { return fmt.Sprintf("%s:%s:%d", v.Kind, v.ID, v.Version) })
	if err != nil {
		return s, err
	}
	s.Verifications, err = doctorList(r, "verifications", planID, func(v domain.ConfigurationVerification) string { return v.ID })
	if err != nil {
		return s, err
	}
	if s.Plan != nil {
		seen := map[string]bool{}
		for _, v := range s.Verifications {
			matched := false
			for _, op := range s.Plan.Operations {
				matched = matched || v.PlanID == s.Plan.ID && v.OperationID == op.ID && v.Scope == op.Scope && v.ResourceVersion == op.ExpectedResourceVersion+1 && op.Active
			}
			if !matched || seen[v.OperationID] {
				return s, errors.New("observation_inconsistent")
			}
			seen[v.OperationID] = true
		}
		var latest struct {
			Data domain.ConfigurationPlan `json:"data"`
		}
		if err := r.read("plans", "get", planID, "", "", &latest); err != nil {
			return s, err
		}
		if !reflect.DeepEqual(latest.Data, *s.Plan) {
			return s, errors.New("configuration_changed")
		}
	}
	var latest struct {
		Data domain.ConfigurationCatalog `json:"data"`
	}
	if err := r.read("catalog", "", "", "", "", &latest); err != nil {
		return s, err
	}
	if !reflect.DeepEqual(latest.Data, s.Catalog) {
		return s, errors.New("configuration_changed")
	}
	return s, nil
}

func runDoctorClient(ctx context.Context, args []string, out io.Writer) error {
	usage := errors.New("usage: platform doctor [--url ORIGIN] [--credentials PRIVATE_FILE] [--plan-id ID] [--timeout-seconds 1..120] [--json] | platform doctor --catalog [--json]")
	f := flag.NewFlagSet("doctor", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	base := f.String("url", os.Getenv("PLATFORM_URL"), "platform HTTPS origin or explicit loopback development HTTP")
	credentials := f.String("credentials", "", "private current credentials file")
	planID := f.String("plan-id", "", "inspect one exact plan and its verification history")
	timeout := f.Int("timeout-seconds", 30, "total inspection deadline,1..120 seconds")
	compact := f.Bool("json", false, "machine-readable JSON")
	catalog := f.Bool("catalog", false, "local remediation catalog; no credentials or network")
	if f.Parse(args) != nil || f.NArg() != 0 || *timeout < 1 || *timeout > 120 || *planID != "" && !configurationCLIIdentifier.MatchString(*planID) {
		return usage
	}
	invalid := false
	f.Visit(func(v *flag.Flag) {
		if *catalog && v.Name != "catalog" && v.Name != "json" {
			invalid = true
		}
	})
	if invalid {
		return usage
	}
	enc := json.NewEncoder(out)
	if !*compact {
		enc.SetIndent("", "  ")
	}
	if *catalog {
		return enc.Encode(doctor.Remediations())
	}
	r := doctor.New(time.Now())
	identity, err := configurationCLIIdentity(*credentials)
	if err != nil {
		r.Add("credentials_unavailable", "attention", "", "Не удалось безопасно прочитать текущие учётные данные. Секреты не включены в отчёт.", "inspect_access")
	} else {
		bounded, cancel := context.WithTimeout(ctx, time.Duration(*timeout)*time.Second)
		defer cancel()
		var snapshot doctor.Snapshot
		snapshot, err = (doctorReader{bounded, *base, identity}).collect(*planID)
		if err == nil {
			doctor.Evaluate(&r, snapshot)
		} else {
			code := err.Error() // Only fixed codes produced by doctorReader/doctorList.
			if bounded.Err() != nil {
				code = "inspection_deadline"
			}
			remedy := "inspect_access"
			if slices.Contains([]string{"configuration_changed", "pagination_inconsistent", "observation_inconsistent", "inspection_limit_reached", "inspection_deadline"}, code) {
				remedy = "repeat_inspection"
			}
			r.Add(code, "unknown", "", "Диагностика не завершена. Нельзя делать вывод об отсутствии настроек или неисправностей; внешние проверки и исправления не выполнялись.", remedy)
		}
	}
	r.Finish(err == nil, time.Now())
	if e := enc.Encode(r); e != nil {
		return e
	}
	if r.Status != "observed" {
		return errors.New("doctor requires attention; inspect the structured report and remediation catalog")
	}
	return nil
}
