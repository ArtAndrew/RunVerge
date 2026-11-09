//go:build !darwin && !linux

// SPDX-License-Identifier: Elastic-2.0
package recoveryaccess

import "context"

// Do not substitute weaker locking or permission semantics on other platforms.
type hostDirectory struct{}

func createHostDirectory(context.Context, string) (*hostDirectory, error) { return nil, ErrHostState }
func openHostDirectory(context.Context, string, bool) (*hostDirectory, error) {
	return nil, ErrHostState
}
func (*hostDirectory) close()                                {}
func (*hostDirectory) writeState(hostState) error            { return ErrHostState }
func (*hostDirectory) writeIntent(hostIntent) error          { return ErrHostState }
func (*hostDirectory) finish(hostIntent) error               { return ErrHostState }
func (*hostDirectory) syncParent() error                     { return ErrHostState }
func (*hostDirectory) load() (hostState, *hostIntent, error) { return hostState{}, nil, ErrHostState }
func SaveHostToken(string, string) error                     { return ErrHostState }
func LoadHostToken(string) (string, error)                   { return "", ErrHostState }
