// SPDX-License-Identifier: Elastic-2.0
package recoveryprocess

import (
	"context"
	"time"
)

type ObservedSourceCapture struct{ record SourceStopRecord }

func (o ObservedSourceCapture) Record() SourceStopRecord { return cloneSourceStop(o.record) }

// ObserveSourceCapture records exact process generations after executor stop,
// before database export. This read-only observation is not a final source stop
// or a claim that already sent external effects have finished.
func (c *Client) ObserveSourceCapture(ctx context.Context, p Intent, b SourceBinding) (ObservedSourceCapture, error) {
	if c == nil || ctx == nil {
		return ObservedSourceCapture{}, ErrProfile
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r := cloneSourceStop(SourceStopRecord{Intent: p, Binding: b})
	o, err := c.InspectBoundSource(ctx, r.Intent, r.Binding)
	r.Observation = o
	if err != nil || ctx.Err() != nil || !SourceCaptureRecordConsistent(r) {
		return ObservedSourceCapture{}, ErrProfile
	}
	return ObservedSourceCapture{cloneSourceStop(r)}, nil
}
