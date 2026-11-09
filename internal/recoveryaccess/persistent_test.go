//go:build darwin || linux

// SPDX-License-Identifier: Elastic-2.0
package recoveryaccess

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hostFixture(t *testing.T) (string, HostInfo) {
	t.Helper()
	p, c, _, _ := fixture(t)
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "authority")
	info, err := InitHost(context.Background(), path, p.Binding, []Scope{{c.Subject, c.OrganizationID}}, "current-operator:test")
	if err != nil {
		t.Fatal("host init failed", err)
	}
	return path, info
}

func TestHostPolicyRestartAndRevocation(t *testing.T) {
	path, initial := hostFixture(t)
	ctx := context.Background()
	scope := initial.Scopes[0]
	token, err := IssueHost(ctx, path, scope.Subject, scope.OrganizationID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	read := func(token string) error {
		return WithHostAuthority(ctx, path, func(ctx context.Context, a *Authority) error {
			return a.Read(ctx, token, scope.OrganizationID, func(context.Context, Claims) error { return nil })
		})
	}
	if err = read(token); err != nil {
		t.Fatal("issued permit rejected", err)
	}
	for _, callbackError := range []error{nil, errors.New("synthetic callback failure")} {
		var retained *Authority
		_ = WithHostAuthority(ctx, path, func(_ context.Context, a *Authority) error { retained = a; return callbackError })
		if retained == nil || retained.Read(ctx, token, scope.OrganizationID, func(context.Context, Claims) error { t.Fatal("escaped authority entered callback"); return nil }) != ErrDenied {
			t.Fatal("escaped transient authority remained usable")
		}
		stored, _ := decodeState(mustReadHostTestFile(t, filepath.Join(path, "access.json")))
		policy, _ := stored.policy()
		policy.Epoch++
		if retained.Replace(policy) != ErrDenied {
			t.Fatal("closed authority revived by replacement")
		}
	}
	// Every public operation opens a new file/authority instance; this is not an
	// in-memory epoch surviving by accident.
	if _, err = InitHost(ctx, path, initial.Binding, initial.Scopes, "again"); err == nil {
		t.Fatal("existing authority overwritten")
	}
	replaced, err := ReplaceHost(ctx, path, 1, initial.Scopes, "current-operator:rotation")
	if err != nil || replaced.Epoch != 2 || replaced.OperationID == initial.OperationID || replaced.PublicKeySHA256 == initial.PublicKeySHA256 {
		t.Fatal("rotation failed", err)
	}
	if read(token) != ErrDenied {
		t.Fatal("old key accepted after reopen")
	}
	newToken, err := IssueHost(ctx, path, scope.Subject, scope.OrganizationID, time.Minute)
	if err != nil || read(newToken) != nil {
		t.Fatal("fresh permit rejected", err)
	}
	if _, err = ReplaceHost(ctx, path, 1, nil, "stale"); err == nil {
		t.Fatal("stale epoch replaced current policy")
	}
	if _, err = ReplaceHost(ctx, path, 2, nil, "current-operator:revocation"); err != nil {
		t.Fatal(err)
	}
	if _, err = IssueHost(ctx, path, scope.Subject, scope.OrganizationID, time.Minute); err != ErrDenied {
		t.Fatal("revoked scope issued token")
	}
	if read(newToken) != ErrDenied {
		t.Fatal("revoked permit accepted")
	}
	inspection, err := InspectHost(ctx, path)
	if err != nil || inspection.State != "active" || inspection.Current.Epoch != 3 || inspection.Pending != nil {
		t.Fatal("durable inspection failed", err)
	}
}

func mustReadHostTestFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestHostPendingFaultBoundaries(t *testing.T) {
	for _, phase := range []string{"intent_file_synced", "intent_directory_synced", "resolution_intent_synced", "state_file_synced", "state_renamed", "state_directory_synced", "intent_removed", "intent_removal_synced"} {
		t.Run(phase, func(t *testing.T) {
			path, initial := hostFixture(t)
			ctx := context.Background()
			scope := initial.Scopes[0]
			oldToken, err := IssueHost(ctx, path, scope.Subject, scope.OrganizationID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			d, err := openHostDirectory(ctx, path, true)
			if err != nil {
				t.Fatal(err)
			}
			s, _, err := d.load()
			if err != nil {
				d.close()
				t.Fatal(err)
			}
			next, err := freshState(s.Info.Binding, 2, nil, "revoke:fixture")
			if err != nil {
				d.close()
				t.Fatal(err)
			}
			i := hostIntent{PreviousSHA256: stateHash(stateBytes(s)), Next: next}
			reached := false
			d.fault = func(at string) error {
				if at == phase {
					reached = true
					return errors.New("synthetic durability boundary fault")
				}
				return nil
			}
			err = d.writeIntent(i)
			if err == nil {
				err = d.finish(i)
			}
			d.close()
			if err == nil || !reached {
				t.Fatal("fault boundary not reached")
			}
			inspection, err := InspectHost(ctx, path)
			if err != nil {
				t.Fatal("inspect failed", err)
			}
			if inspection.State == "pending" {
				if inspection.Pending == nil || inspection.Pending.OperationID != next.Info.OperationID {
					t.Fatal("pending identity lost")
				}
				if _, err = IssueHost(ctx, path, scope.Subject, scope.OrganizationID, time.Minute); err != ErrPending {
					t.Fatal("pending state issued token")
				}
				entered := false
				if WithHostAuthority(ctx, path, func(context.Context, *Authority) error { entered = true; return nil }) != ErrPending || entered {
					t.Fatal("pending state entered reader")
				}
				if _, err = ReplaceHost(ctx, path, inspection.Current.Epoch, nil, "repeat"); err != ErrPending {
					t.Fatal("pending change blindly replaced")
				}
				if _, err = ResolveHost(ctx, path, strings.Repeat("0", 32)); err == nil {
					t.Fatal("wrong resolution accepted")
				}
				resolved, e := ResolveHost(ctx, path, next.Info.OperationID)
				if e != nil || resolved.OperationID != next.Info.OperationID {
					t.Fatal("exact explicit resolution failed", e)
				}
			} else if inspection.Current.OperationID != next.Info.OperationID {
				t.Fatal("unfenced old policy after durable intent")
			}
			if err = WithHostAuthority(ctx, path, func(ctx context.Context, a *Authority) error {
				return a.Read(ctx, oldToken, scope.OrganizationID, func(context.Context, Claims) error { t.Fatal("old permit entered callback"); return nil })
			}); err != ErrDenied {
				t.Fatal("resolved change restored old authority", err)
			}
		})
	}
}

func TestHostRejectsUnsafeOrMissingState(t *testing.T) {
	for _, fault := range []string{"missing_lock", "missing_state", "state_symlink", "lock_symlink", "state_hardlink", "state_mode", "parent_mode", "bad_json", "bad_intent"} {
		t.Run(fault, func(t *testing.T) {
			path, initial := hostFixture(t)
			file := filepath.Join(path, "access.json")
			lock := filepath.Join(path, "access.lock")
			switch fault {
			case "missing_lock":
				if err := os.Remove(lock); err != nil {
					t.Fatal(err)
				}
			case "missing_state":
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			case "state_symlink", "lock_symlink":
				target := file
				if fault == "lock_symlink" {
					target = lock
				}
				if err := os.Rename(target, target+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Base(target)+".saved", target); err != nil {
					t.Fatal(err)
				}
			case "state_hardlink":
				if err := os.Link(file, file+".link"); err != nil {
					t.Fatal(err)
				}
			case "state_mode":
				if err := os.Chmod(file, 0644); err != nil {
					t.Fatal(err)
				}
			case "parent_mode":
				if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
			case "bad_json":
				if err := os.WriteFile(file, []byte(`{"info":null}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "bad_intent":
				if err := os.WriteFile(filepath.Join(path, "pending.json"), []byte("partial"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := InspectHost(context.Background(), path); err == nil {
				t.Fatal("unsafe/missing state accepted")
			}
			if _, err := InitHost(context.Background(), path, initial.Binding, initial.Scopes, "reset"); err == nil {
				t.Fatal("invalid directory silently reset")
			}
		})
	}
}

func TestHostProcessHelper(t *testing.T) {
	path := os.Getenv("RUNVERGE_HOST_POLICY_TEST_PATH")
	mode := os.Getenv("RUNVERGE_HOST_POLICY_TEST_MODE")
	if path == "" || mode == "" {
		return
	}
	ctx := context.Background()
	if mode == "hold" {
		err := WithHostAuthority(ctx, path, func(context.Context, *Authority) error {
			fmt.Println("HOST_LOCK_HELD")
			_, err := bufio.NewReader(os.Stdin).ReadString('\n')
			return err
		})
		if err != nil {
			os.Exit(21)
		}
		os.Exit(0)
	}
	if mode == "crash_after_rename" {
		d, err := openHostDirectory(ctx, path, true)
		if err != nil {
			os.Exit(22)
		}
		s, _, err := d.load()
		if err != nil {
			os.Exit(23)
		}
		next, err := freshState(s.Info.Binding, s.Info.Epoch+1, nil, "process-crash:test")
		if err != nil {
			os.Exit(24)
		}
		i := hostIntent{PreviousSHA256: stateHash(stateBytes(s)), Next: next}
		if d.writeIntent(i) != nil {
			os.Exit(25)
		}
		d.fault = func(at string) error {
			if at == "state_renamed" {
				os.Exit(73)
			}
			return nil
		}
		_ = d.finish(i)
		os.Exit(26)
	}
	os.Exit(27)
}

func hostChild(t *testing.T, ctx context.Context, path, mode string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestHostProcessHelper$")
	cmd.Env = append(os.Environ(), "RUNVERGE_HOST_POLICY_TEST_PATH="+path, "RUNVERGE_HOST_POLICY_TEST_MODE="+mode)
	return cmd
}

func TestHostCrossProcessBarrierAndCrash(t *testing.T) {
	path, initial := hostFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := hostChild(t, ctx, path, "hold")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "HOST_LOCK_HELD\n" {
		t.Fatal("child did not acquire read lock")
	}
	short, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = ReplaceHost(short, path, 1, nil, "blocked:replacement")
	stop()
	if err == nil {
		t.Fatal("replacement passed an active reader in another process")
	}
	if _, err = stdin.Write([]byte("release\n")); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	if err = cmd.Wait(); err != nil {
		t.Fatal("read child failed", err)
	}
	if _, err = ReplaceHost(ctx, path, 1, initial.Scopes, "after:barrier"); err != nil {
		t.Fatal(err)
	}
	crash := hostChild(t, ctx, path, "crash_after_rename")
	err = crash.Run()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 73 {
		t.Fatal("child did not crash at observed rename boundary")
	}
	inspection, err := InspectHost(ctx, path)
	if err != nil || inspection.State != "pending" || inspection.Pending == nil || inspection.Pending.Epoch != 3 {
		t.Fatal("crash lost durable fence", err)
	}
	if _, err = IssueHost(ctx, path, initial.Scopes[0].Subject, initial.Scopes[0].OrganizationID, time.Minute); err != ErrPending {
		t.Fatal("fresh process issued through crash fence")
	}
	if _, err = ResolveHost(ctx, path, inspection.Pending.OperationID); err != nil {
		t.Fatal("explicit crash resolution failed", err)
	}
	inspection, err = InspectHost(ctx, path)
	if err != nil || inspection.State != "active" || inspection.Current.Epoch != 3 || len(inspection.Current.Scopes) != 0 {
		t.Fatal("resolved crash policy lost", err)
	}
}
