// SPDX-License-Identifier: Elastic-2.0
package domain

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var configurationDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func configurationText(s string, max int) bool {
	return utf8.ValidString(s) && strings.TrimSpace(s) != "" && len(s) <= max && !strings.ContainsRune(s, 0)
}

func ValidateConfigurationReason(reason string) error {
	if !configurationText(reason, 4096) {
		return Err("invalid_request", "Нужна причина длиной до 4096 байт", 400)
	}
	return nil
}

func ConfigurationActionAllowed(action string) bool {
	switch action {
	case "inspect", "plan", "validate", "apply", "verify", "delegate":
		return true
	}
	return false
}

// Shape validation never establishes authority. Current profiles, ancestor
// limits, epochs and exact resource revisions are evaluated in storage.
func ValidateCreateConfigurationGrant(in CreateConfigurationGrant) error {
	if !configurationText(in.SubjectID, 128) || in.TTLSeconds < 1 || in.TTLSeconds > MaxConfigurationGrantTTLSeconds ||
		in.MaxPlans < 1 || in.MaxPlans > MaxConfigurationPlans || in.MaxVerifications < 1 || in.MaxVerifications > MaxConfigurationVerifications ||
		in.MaxAllocatedChargeMicroUSD < 0 || in.MaxAllocatedChargeMicroUSD > MaxMoney ||
		len(in.Actions) < 1 || len(in.Actions) > 6 || len(in.Destinations) < 1 || len(in.Destinations) > MaxConfigurationScopes {
		return Err("invalid_request", "Некорректные границы делегирования настройки", 400)
	}
	if in.ParentGrantID == "" {
		if in.ExpectedParentVersion != 0 {
			return Err("invalid_request", "Корневое разрешение не имеет версии родителя", 400)
		}
	} else if !configurationText(in.ParentGrantID, 128) || in.ExpectedParentVersion < 1 {
		return Err("invalid_request", "Нужна точная версия родительского разрешения", 400)
	}
	actions := map[string]bool{}
	for _, action := range in.Actions {
		if !ConfigurationActionAllowed(action) || actions[action] {
			return Err("invalid_request", "Неизвестное или повторное действие настройки", 400)
		}
		actions[action] = true
	}
	destinations := map[string]bool{}
	for _, destination := range in.Destinations {
		// Multiple versions of the same destination cannot be used to alias a
		// fixed target within one grant. Rotation needs a new explicit grant.
		if !configurationText(destination.ID, 128) || destination.Version < 1 || destination.Version > 2147483647 || destinations[destination.ID] {
			return Err("invalid_request", "Некорректное или повторное назначение настройки", 400)
		}
		destinations[destination.ID] = true
	}
	return ValidateConfigurationReason(in.Reason)
}

func ValidateCreateConfigurationPlan(in CreateConfigurationPlan) error {
	if in.BaseRevision < 1 || in.TTLSeconds < 1 || in.TTLSeconds > MaxConfigurationPlanTTLSeconds ||
		len(in.Operations) < 1 || len(in.Operations) > MaxConfigurationOperations ||
		(in.GrantID != "" && !configurationText(in.GrantID, 128)) {
		return Err("invalid_request", "Некорректные границы плана настройки", 400)
	}
	ids, destinations := map[string]bool{}, map[string]bool{}
	for _, operation := range in.Operations {
		if !configurationText(operation.ID, 128) || !configurationText(operation.DestinationID, 128) ||
			operation.DestinationVersion < 1 || operation.DestinationVersion > 2147483647 || operation.ExpectedResourceVersion < 0 || operation.ExpectedResourceVersion >= 2147483647 ||
			operation.Active == nil || ids[operation.ID] || destinations[operation.DestinationID] {
			return Err("invalid_request", "Нужны уникальные операции, точные версии и явное active", 400)
		}
		ids[operation.ID], destinations[operation.DestinationID] = true, true
	}
	return ValidateConfigurationReason(in.Reason)
}

func ValidateConfigurationSnapshot(version int, digest string) error {
	if version < 1 || version > 2147483647 || !configurationDigest.MatchString(digest) {
		return Err("invalid_request", "Нужны точная версия и SHA-256 плана", 400)
	}
	return nil
}

func ValidateStartConfigurationVerification(in StartConfigurationVerification) error {
	if err := ValidateConfigurationSnapshot(in.ExpectedPlanVersion, in.PlanDigest); err != nil {
		return err
	}
	if !configurationText(in.OperationID, 128) || in.MaxChargeMicroUSD < 0 || in.MaxChargeMicroUSD > MaxMoney ||
		in.AcknowledgeCost == nil || !*in.AcknowledgeCost {
		return Err("invalid_request", "Нужны операция, допустимый предел расходов и явное согласие", 400)
	}
	return ValidateConfigurationReason(in.Reason)
}
