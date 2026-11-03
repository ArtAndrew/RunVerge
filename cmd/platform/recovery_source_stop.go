// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"errors"

	"agentplatform.local/platform/internal/recoveryarchive"
	"agentplatform.local/platform/internal/recoveryprocess"
)

func validateSourceStopPath(saved recoveryProcessFile, path string) error {
	if saved.Capture == nil || saved.Binding == nil || path == "" {
		return errors.New("source evidence capture required")
	}
	for _, archive := range []string{saved.Capture.SelectedDirectory, saved.Capture.Directory} {
		if err := recoveryarchive.RequireHostPathOutside(archive, path); err != nil {
			return err
		}
	}
	return nil
}

// Called after archive verification/cleanup. Neither path retries a stop. A
// partial previous output requires a different create-only output path.
func inspectSourceStop(ctx context.Context, c *recoveryprocess.Client, saved recoveryProcessFile, intentPath, output, input string) (string, recoveryprocess.Observation, error) {
	var zero recoveryprocess.Observation
	path := output
	if input != "" {
		path = input
	}
	if (output == "") == (input == "") || validateSourceStopPath(saved, path) != nil {
		return "", zero, errors.New("one source stop proof path outside both archives required")
	}
	link, err := loadSourceCaptureLink(saved, intentPath)
	if err != nil {
		return "", zero, err
	}
	var fresh recoveryprocess.ObservedSourceStop
	var proof *recoveryarchive.VerifiedSourceStop
	if input != "" {
		proof, err = recoveryarchive.VerifyHostSourceStop(input, saved.Capture.Keys, link)
		if err == nil {
			fresh, err = recoveryarchive.RecheckHostSourceStop(ctx, c, proof, link)
		}
	} else {
		fresh, err = c.ObserveSourceStop(ctx, saved.Intent, *saved.Binding)
		if err == nil {
			err = recoveryarchive.WriteHostSourceStop(output, saved.Capture.Keys, link, fresh)
		}
		if err == nil {
			proof, err = recoveryarchive.VerifyHostSourceStop(output, saved.Capture.Keys, link)
		}
	}
	if err != nil {
		return "", zero, err
	}
	return proof.SHA256(), fresh.Record().Observation, nil
}
