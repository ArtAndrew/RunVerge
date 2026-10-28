// SPDX-License-Identifier: Elastic-2.0
package modelruntime

import "agentplatform.local/platform/internal/domain"

// DescribeIntent lets the trusted kernel recompute admission from its own
// persisted task/agent/profile snapshot rather than trusting a worker's copy.
// It does not perform admission or network I/O and does not contain credentials.
func (r *Runtime) DescribeIntent(lease domain.Lease) (string, string, error) { return r.intent(lease) }
