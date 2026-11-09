// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/storage"
	"agentplatform.local/platform/internal/toolruntime"
)

func runInstallGitHubCIComment(ctx context.Context, s *storage.Store, args []string) error {
	f := flag.NewFlagSet("install-github-ci-comment", flag.ContinueOnError)
	org := f.String("organization", "", "target organization ID")
	repo := f.String("repository", "", "GitHub OWNER/REPOSITORY")
	id := f.String("tool-id", "github-ci-comment", "tool ID")
	version := f.Int("version", 1, "immutable tool version")
	credential := f.String("credential-environment", "", "trusted worker environment reference, not the token")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected command arguments")
	}
	tool := domain.ToolConnection{
		Tool: domain.ToolInfo{
			ID: *id, OrganizationID: *org, Version: *version, Name: "Publish GitHub CI comment",
			Description: "Post the exact approved CI draft on its commit after rechecking the current workflow attempt.",
			Effect:      "write", Destination: "https://github.com/" + *repo, Provider: "github", PriceVersion: toolruntime.GitHubCICommentPriceVersion,
			InputSchemaJSON: toolruntime.GitHubCICommentSchema, InputSchemaDigest: storage.Hash([]byte(toolruntime.GitHubCICommentSchema)),
			RequireDistinctApprover: true, TimeoutSeconds: 8, MaxInputBytes: domain.MaxToolArgumentsBytes,
			MaxOutputBytes: 16384, Active: true,
		},
		Transport: "http_json", ResultContract: domain.ToolResultContractGitHubCICommentV1,
		Endpoint: "https://api.github.com/repos/" + *repo, CredentialEnvironment: *credential,
	}
	installed, err := s.InstallToolConnection(ctx, tool)
	if err != nil {
		return errors.New("GitHub CI comment tool installation rejected; verify repository, organization, version and credential reference")
	}
	fmt.Printf("GitHub CI comment tool installed: %s version %d\n", installed.Tool.ID, installed.Tool.Version)
	return nil
}
