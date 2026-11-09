// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"runtime"

	"agentplatform.local/platform/internal/recoverycontrol"
	"agentplatform.local/platform/internal/storage"
)

func startRecoveryControl(ctx context.Context, s *storage.Store, command string) (func(), error) {
	mode := os.Getenv("RUNVERGE_RECOVERY_CONTROL")
	if mode == "" {
		return func() {}, nil
	}
	if mode != "container_v1" || runtime.GOOS != "linux" || os.Getpid() != 1 || (command != "serve" && command != "worker") {
		return nil, errors.New("recovery control requires the explicit Linux container runtime profile")
	}
	role := "worker"
	if command == "serve" {
		role = "api"
	}
	server, err := recoverycontrol.Start(ctx, recoverycontrol.Socket, role, s.HoldRecoverySourceWitness)
	if err != nil {
		return nil, err
	}
	return server.Close, nil
}

func runRecoverySourceWitness(ctx context.Context, args []string, out io.Writer) error {
	f := flag.NewFlagSet("source-witness", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	source := f.String("expected-installation", "", "exact source installation")
	nonce := f.String("nonce", "", "fresh challenge")
	role := f.String("role", "", "api or worker")
	if f.Parse(args) != nil || f.NArg() != 0 {
		return recoverycontrol.ErrWitness
	}
	reply, err := recoverycontrol.Probe(ctx, recoverycontrol.Socket, *role, *source, *nonce)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(reply)
}
