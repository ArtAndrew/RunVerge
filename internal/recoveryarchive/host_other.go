//go:build !darwin && !linux

// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive

import (
	"context"
	"os"
)

type hostBundle struct{}

func writeHostPrivateFile(string, []byte) error                   { return ErrHostFiles }
func readHostPrivateFile(string, int64) ([]byte, error)           { return nil, ErrHostFiles }
func newHostBundle(context.Context, string) (*hostBundle, error)  { return nil, ErrHostFiles }
func openHostBundle(context.Context, string) (*hostBundle, error) { return nil, ErrHostFiles }
func (*hostBundle) close()                                        {}
func (*hostBundle) create(string) (*os.File, error)               { return nil, ErrHostFiles }
func (*hostBundle) open(string) (*os.File, error)                 { return nil, ErrHostFiles }
func (*hostBundle) read(string, int64) ([]byte, error)            { return nil, ErrHostFiles }
func (*hostBundle) write(string, []byte) error                    { return ErrHostFiles }
func (*hostBundle) sync() error                                   { return ErrHostFiles }
