// SPDX-License-Identifier: Elastic-2.0
// Package toolruntime sends one explicitly approved external occurrence. It is
// neither a model tool-selection loop nor an operating-system sandbox.
package toolruntime

import (
	"agentplatform.local/platform/internal/domain"
	"context"
	"crypto/x509"
	"time"
)

type Store interface {
	PrepareToolDispatch(context.Context, *domain.ToolLease) (domain.ToolDispatchPermit, error)
	MarkCostUnknown(context.Context, string, string, string, string) (domain.Reservation, error)
	RecordToolOutcome(context.Context, domain.ToolOutcomeEvidence) (domain.ToolOutcomeReceipt, error)
}
type Config struct {
	Connection         domain.ToolConnection
	Credential         string
	TrustedRoots       *x509.CertPool
	PersistenceTimeout time.Duration
}

func (Config) String() string   { return "toolruntime.Config{redacted}" }
func (Config) GoString() string { return "toolruntime.Config{redacted}" }

type Outcome struct {
	Status    string
	ReceiptID string
}
type Error struct{ Code string }

func (e *Error) Error() string { return "tool runtime: " + e.Code }
func failed(code string) error { return &Error{Code: code} }
