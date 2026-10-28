//go:build !darwin && !linux

// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive

import "os"

// No fallback to world-readable or linked temporary files on other platforms.
type staging struct{}

func newStaging(string) (*staging, error)               { return nil, ErrOptions }
func (*staging) anonymousFile(string) (*os.File, error) { return nil, ErrOptions }
func (*staging) Close() error                           { return nil }
