// SPDX-License-Identifier: Elastic-2.0
package domain

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var webhookIdentifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func ValidateWebhookReason(reason string) error {
	if !utf8.ValidString(reason) || len(reason) > 4096 || strings.TrimSpace(reason) == "" || strings.ContainsRune(reason, 0) {
		return Err("invalid_request", "Нужна причина длиной до 4096 байт", 400)
	}
	return nil
}

func ValidateCreateWebhookSubscription(in CreateWebhookSubscription) error {
	if !webhookIdentifier.MatchString(in.DestinationID) || in.DestinationVersion < 1 || in.DestinationVersion > 2147483647 ||
		in.TTLSeconds < 1 || in.TTLSeconds > MaxWebhookTTLSeconds ||
		in.MaxNotifications < 1 || in.MaxNotifications > MaxWebhookNotifications ||
		!in.AcknowledgeMetadataExport || len(in.EventTypes) < 1 || len(in.EventTypes) > MaxWebhookEventTypes {
		return Err("invalid_request", "Нужны точное назначение, предел уведомлений, срок и согласие на экспорт метаданных", 400)
	}
	registry := eventPayloadDefinitions()
	seen := map[string]bool{}
	for _, kind := range in.EventTypes {
		if _, registered := registry[kind]; !registered || seen[kind] {
			return Err("invalid_request", "Нужны различные зарегистрированные типы событий", 400)
		}
		seen[kind] = true
	}
	return ValidateWebhookReason(in.Reason)
}
