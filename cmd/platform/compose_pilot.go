// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/storage"
)

const (
	pilotStateDir   = "/state"
	pilotRuntimeDir = "/pilot-runtime"
	pilotDBDir      = "/pilot-db"
	pilotModel      = "qwen2.5-1.5b-instruct"
	pilotAgent      = "Pilot · Draft an announcement"
)

type pilotPasswords struct {
	Admin   string `json:"admin"`
	App     string `json:"app"`
	Profile string `json:"profile"`
	Salt    string `json:"salt"`
}

type pilotDatabase struct {
	Password string `json:"password"`
}

func pilotHex(value string, size int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == size && strings.ToLower(value) == value
}

func pilotEntropy(size int) string {
	bytes := make([]byte, size)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

func withPilotDirectory(path string, lock bool, action func(*os.Root, *os.File) error) error {
	before, err := os.Lstat(path)
	if err != nil || !before.IsDir() || before.Mode().Perm()&0077 != 0 ||
		before.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return errors.New("Compose pilot requires its original private volumes")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return errors.New("cannot open the private Compose pilot volume")
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return errors.New("cannot open the private Compose pilot directory")
	}
	defer dir.Close()
	after, err := dir.Stat()
	if err != nil || !os.SameFile(before, after) {
		return errors.New("Compose pilot volume changed")
	}
	if lock && syscall.Flock(int(dir.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return errors.New("another Compose pilot setup holds this volume")
	}
	return action(root, dir)
}

// Write once, then require the same bytes. A stopped initializer can finish
// copying from its original intent without rotating any live DB password.
func ensurePilotFile(root *os.Root, dir *os.File, name, content string) error {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return publishBootstrapFile(root, dir, name, func(w io.Writer) error {
			_, err := io.WriteString(w, content)
			return err
		})
	}
	if err == nil {
		defer file.Close()
		info, statErr := file.Stat()
		if statErr == nil && info.Mode().IsRegular() && info.Mode().Perm()&0077 == 0 &&
			info.Sys().(*syscall.Stat_t).Uid == uint32(os.Geteuid()) {
			actual, readErr := io.ReadAll(io.LimitReader(file, int64(len(content)+1)))
			if readErr == nil && string(actual) == content {
				return dir.Sync()
			}
		}
	}
	return errors.New("Compose pilot credentials do not match the original private settings")
}

func prepareComposePilot() error {
	return withPilotDirectory(pilotStateDir, true, func(root *os.Root, dir *os.File) error {
		const intent = "pilot-passwords.json"
		var settings pilotPasswords
		if _, err := root.Lstat(intent); os.IsNotExist(err) {
			settings = pilotPasswords{Admin: pilotEntropy(32), App: pilotEntropy(32), Profile: pilotEntropy(16), Salt: pilotEntropy(16)}
			if err := publishBootstrapJSON(root, dir, intent, settings); err != nil {
				return errors.New("cannot save Compose pilot credentials")
			}
		} else if err := readBootstrapJSON(root, intent, &settings); err != nil {
			return errors.New("cannot read the original Compose pilot settings")
		}
		if !pilotHex(settings.Admin, 32) || !pilotHex(settings.App, 32) || settings.Admin == settings.App ||
			!pilotHex(settings.Profile, 16) || !pilotHex(settings.Salt, 16) {
			return errors.New("invalid Compose pilot credentials")
		}
		salt, _ := hex.DecodeString(settings.Salt)
		key, err := pbkdf2.Key(sha256.New, settings.App, salt, 4096, 32)
		if err != nil {
			return errors.New("cannot prepare PostgreSQL credentials")
		}
		derive := func(label string) []byte {
			mac := hmac.New(sha256.New, key)
			mac.Write([]byte(label))
			return mac.Sum(nil)
		}
		client := sha256.Sum256(derive("Client Key"))
		b64 := base64.StdEncoding.EncodeToString
		verifier := "SCRAM-SHA-256$4096:" + b64(salt) + "$" + b64(client[:]) + ":" + b64(derive("Server Key"))
		if err := withPilotDirectory(pilotDBDir, false, func(root *os.Root, dir *os.File) error {
			for _, item := range []struct{ name, value string }{
				{"password", settings.Admin}, {"verifier", verifier}, {"profile", settings.Profile},
			} {
				if err := ensurePilotFile(root, dir, item.name, item.value+"\n"); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		return withPilotDirectory(pilotRuntimeDir, false, func(root *os.Root, dir *os.File) error {
			data, _ := json.Marshal(pilotDatabase{Password: settings.App})
			return ensurePilotFile(root, dir, "database.json", string(data)+"\n")
		})
	})
}

func composePilotDSN(environmentDSN string) (string, error) {
	if environmentDSN != "" {
		return "", errors.New("Compose pilot uses its own database. Remove DATABASE_URL from the service")
	}
	var cfg pilotDatabase
	err := withPilotDirectory(pilotRuntimeDir, false, func(root *os.Root, _ *os.File) error {
		return readBootstrapJSON(root, "database.json", &cfg)
	})
	if err != nil || !pilotHex(cfg.Password, 32) {
		return "", errors.New("Compose pilot database settings are unavailable")
	}
	return "postgres://runverge:" + cfg.Password + "@db:5432/runverge?sslmode=disable", nil
}

type pilotMemberLogin struct {
	OrganizationID string    `json:"organization_id"`
	PrincipalID    string    `json:"principal_id"`
	Token          string    `json:"token"`
	ExpiresAt      time.Time `json:"expires_at"`
}

func seedComposePilot(ctx context.Context, s *storage.Store) error {
	if err := runDedicatedBootstrap(ctx, s, []string{"--state-dir", pilotStateDir, "--name", "RunVerge Team Pilot"}, io.Discard); err != nil {
		return err
	}
	return withPilotDirectory(pilotStateDir, true, func(root *os.Root, dir *os.File) error {
		var owner storage.Bootstrap
		if err := readBootstrapJSON(root, "bootstrap.json", &owner); err != nil {
			return errors.New("Compose pilot owner handoff is unavailable")
		}
		const completed = "pilot-ready.json"
		var receipt struct {
			OrganizationID string `json:"organization_id"`
		}
		if _, err := root.Lstat(completed); !os.IsNotExist(err) {
			if err := readBootstrapJSON(root, completed, &receipt); err != nil || receipt.OrganizationID != owner.OrganizationID {
				return errors.New("Compose pilot completion record does not match")
			}
			return nil // Never recreate revoked credentials or replace a team's configuration.
		}
		actor, err := s.Authenticate(ctx, owner.Token, owner.OrganizationID)
		if err != nil {
			return errors.New("cannot prepare the pilot using its original owner")
		}
		model := domain.ModelConnection{
			ID: "pilot_local", OrganizationID: owner.OrganizationID, Version: 1, Model: pilotModel, Active: true,
			Endpoint: "http://127.0.0.1:11434/v1/chat/completions", AllowInsecureLoopback: true,
			CredentialEnvironment: "PLATFORM_MODEL_KEY_LOCAL", TokenLimitField: "max_tokens",
			AccountingMode: domain.ModelAccountingLocalUnmetered, ProviderAccount: "runverge-pilot-local",
			PriceVersion: "pilot-local-v1", BoundAssumptions: "Local CPU inference. No provider tariff. Hardware and electricity are not measured.",
			MaxInputTokens: 2048, MaxOutputTokens: 256, MaxInputBytes: 8192, MaxOutputBytes: 16384, TimeoutMilliseconds: 150000,
		}
		if _, err := s.InstallModelConnection(ctx, model); err != nil {
			return errors.New("cannot install the Compose pilot model")
		}
		budget := int64(1_000_000) // Matches the first-task form's default task limit.
		draft, err := s.CreateAgent(ctx, actor, "compose-pilot-v1-agent", domain.CreateAgent{
			Name: pilotAgent, Model: pilotModel, BudgetMicroUSD: &budget,
			Instructions: "Write the requested short draft using only the facts supplied in the task. Keep dates and numbers exact. Answer in the language of the task. Say what is missing rather than inventing facts. Return a draft for a person to review.",
		})
		var agent domain.Agent
		if err != nil || json.Unmarshal(draft, &agent) != nil || agent.ID == "" {
			return errors.New("cannot create the Compose pilot starter agent")
		}
		if _, err := s.PublishAgent(ctx, actor, agent.ID, "compose-pilot-v1-publish", agent.Version); err != nil {
			return errors.New("cannot publish the Compose pilot starter agent")
		}
		if err := seedPilotMember(ctx, s, actor, root, dir, "compose-pilot-v1-member", "contributor", "Pilot contributor"); err != nil {
			return err
		}
		if err := seedPilotMember(ctx, s, actor, root, dir, "compose-pilot-v2-reviewer", "reviewer", "Pilot reviewer"); err != nil {
			return err
		}
		receipt.OrganizationID = owner.OrganizationID
		return publishBootstrapJSON(root, dir, completed, receipt)
	})
}

func seedPilotMember(ctx context.Context, s *storage.Store, actor domain.Principal, root *os.Root, dir *os.File, key, account, displayName string) error {
	raw, err := s.CreateMember(ctx, actor, key, domain.CreateOrganizationMember{
		DisplayName: displayName, Role: "member", Reason: "Separate " + account + " for the Compose pilot",
	})
	var member domain.OrganizationMember
	if err != nil || json.Unmarshal(raw, &member) != nil || member.PrincipalID == "" {
		return errors.New("cannot create the Compose pilot " + account)
	}
	loginName := account + ".json"
	var login pilotMemberLogin
	if _, err := root.Lstat(loginName); os.IsNotExist(err) {
		token, expires, err := s.IssueMemberToken(ctx, actor.OrganizationID, member.PrincipalID, 24*time.Hour)
		if err != nil {
			return errors.New("cannot issue the initial pilot credential for " + account)
		}
		login = pilotMemberLogin{OrganizationID: actor.OrganizationID, PrincipalID: member.PrincipalID, Token: token, ExpiresAt: expires}
		if err := publishBootstrapJSON(root, dir, loginName, login); err != nil {
			// If the final link exists, keep its exact private credential for retry.
			if _, statErr := root.Lstat(loginName); os.IsNotExist(statErr) {
				revokeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if s.RevokeIssuedToken(revokeCtx, storage.Hash([]byte(token))) != nil {
					return errors.New("pilot credential save failed and revocation could not be confirmed")
				}
			}
			return errors.New("cannot save the private Compose pilot handoff for " + account)
		}
	} else if readBootstrapJSON(root, loginName, &login) != nil || login.OrganizationID != actor.OrganizationID ||
		login.PrincipalID != member.PrincipalID || login.Token == "" || login.ExpiresAt.IsZero() {
		return errors.New("Compose pilot handoff does not match " + account)
	}
	return nil
}
