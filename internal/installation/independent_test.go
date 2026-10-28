// SPDX-License-Identifier: Elastic-2.0
package installation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type independentBundleFixture struct {
	directory string
	manifest  Manifest
	identity  Identity
	hash      string
	executed  string
}

func independentBundleHash(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func independentBundle(t *testing.T) independentBundleFixture {
	t.Helper()
	base := t.TempDir()
	directory := filepath.Join(base, "source")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	executed := filepath.Join(base, "payload-was-executed")
	identity := Identity{APIVersion: "0.14.0", OS: runtime.GOOS, Arch: runtime.GOARCH, SourceSHA256: independentBundleHash([]byte("independent source snapshot")), Migrations: []Migration{{Name: "migrations/001_fixture.sql", SHA256: independentBundleHash([]byte("immutable first migration"))}, {Name: "migrations/002_fixture.sql", SHA256: independentBundleHash([]byte("immutable second migration"))}}}
	m := Manifest{Format: Format, Product: "RunVerge", Version: "0.14.0-alpha.independent", Identity: identity}
	files := map[string][]byte{
		"bin/platform":      []byte("#!/bin/sh\nprintf executed > '" + strings.ReplaceAll(executed, "'", "'\\''") + "'\nexit 97\n"),
		"web/index.html":    []byte("<!doctype html><title>Independent fixture</title>"),
		"web/assets/app.js": []byte("console.log('synthetic fixture');"),
		"LICENSE":           []byte("Synthetic review fixture, no release license claim\n"),
		"components.json":   []byte("{\"synthetic\":true}\n"),
		"source-files.json": []byte("{\"fixture\":\"bounded\"}\n"),
		"RUNNING.md":        []byte("No commands are run by installation.\n"),
	}
	for name, body := range files {
		mode := uint32(0644)
		if name == "bin/platform" {
			mode = 0755
		}
		full := filepath.Join(directory, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, body, os.FileMode(mode)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(full, os.FileMode(mode)); err != nil {
			t.Fatal(err)
		}
		m.Files = append(m.Files, File{Path: name, SHA256: independentBundleHash(body), Size: int64(len(body)), Mode: mode})
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	// Keep expected migration identity independent of mutable manifest copies.
	identity.Migrations = append([]Migration{}, m.Identity.Migrations...)
	f := independentBundleFixture{directory: directory, manifest: m, identity: identity, executed: executed}
	f.hash = independentBundleWriteManifest(t, f.directory, m)
	return f
}

func independentBundleWriteRaw(t *testing.T, directory string, raw []byte) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, ManifestName), raw, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(directory, ManifestName), 0644); err != nil {
		t.Fatal(err)
	}
	return independentBundleHash(raw)
}
func independentBundleWriteManifest(t *testing.T, directory string, m Manifest) string {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return independentBundleWriteRaw(t, directory, raw)
}
func independentBundleClone(t *testing.T, m Manifest) Manifest {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var copy Manifest
	if err = json.Unmarshal(raw, &copy); err != nil {
		t.Fatal(err)
	}
	return copy
}

func independentBundleRejected(t *testing.T, f independentBundleFixture, hash string) {
	t.Helper()
	if _, err := Inspect(f.directory, hash, f.identity, false); !errors.Is(err, ErrInvalid) {
		t.Fatal("malformed bundle did not fail with fixed error", err)
	}
	destination := filepath.Join(t.TempDir(), "new-install")
	if _, err := Install(f.directory, destination, hash, f.identity); err == nil {
		t.Fatal("invalid bundle installed")
	}
	if _, err := os.Lstat(filepath.Join(destination, MarkerName)); !os.IsNotExist(err) {
		t.Fatal("invalid bundle acquired completion marker")
	}
}

func TestIndependentNativeBundleValidOfflineCopyAndPinnedVerification(t *testing.T) {
	f := independentBundle(t)
	t.Setenv("DATABASE_URL", "invalid-database-setting-never-consulted")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1/")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1/")
	before, err := Inspect(f.directory, f.hash, f.identity, false)
	if err != nil {
		t.Fatal(err)
	}
	if before.Installed || before.PublisherAuthenticated || before.FileCount != len(f.manifest.Files) {
		t.Fatal("inspection overstated completion/authenticity")
	}
	destination := filepath.Join(t.TempDir(), "native-install")
	after, err := Install(f.directory, destination, f.hash, f.identity)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Installed || after.PublisherAuthenticated || after.ManifestSHA256 != f.hash {
		t.Fatal("completion report is not bound to exact manifest")
	}
	for _, item := range f.manifest.Files {
		raw, e := os.ReadFile(filepath.Join(destination, filepath.FromSlash(item.Path)))
		if e != nil || independentBundleHash(raw) != item.SHA256 || int64(len(raw)) != item.Size {
			t.Fatal("copied content changed", item.Path, e)
		}
		st, e := os.Stat(filepath.Join(destination, filepath.FromSlash(item.Path)))
		if e != nil || st.Mode() != os.FileMode(item.Mode) {
			t.Fatal("copied mode changed", item.Path, e)
		}
	}
	markerInfo, err := os.Stat(filepath.Join(destination, MarkerName))
	if err != nil || markerInfo.Mode() != 0600 {
		t.Fatal("completion marker permissions changed", err)
	}
	if _, err = Inspect(destination, f.hash, f.identity, true); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(f.executed); !os.IsNotExist(err) {
		t.Fatal("installer executed packaged payload")
	}
	if _, err = Inspect(destination, independentBundleHash([]byte("wrong release pin")), f.identity, true); !errors.Is(err, ErrInvalid) {
		t.Fatal("wrong external release pin accepted", err)
	}
	if _, err = Inspect(f.directory, f.hash, f.identity, true); err == nil {
		t.Fatal("uninstalled source treated as complete")
	}
}

func TestIndependentNativeBundleStrictJSON(t *testing.T) {
	for name, change := range map[string]func([]byte) []byte{
		"duplicate": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"product":"RunVerge"`), []byte(`"product":"shadow","product":"RunVerge"`), 1)
		},
		"case_alias": func(b []byte) []byte { return bytes.Replace(b, []byte(`"product":`), []byte(`"Product":`), 1) },
		"unknown":    func(b []byte) []byte { return append([]byte(`{"secret":"not accepted",`), b[1:]...) },
		"null": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"product":"RunVerge"`), []byte(`"product":null`), 1)
		},
		"missing":          func(b []byte) []byte { return bytes.Replace(b, []byte(`"product":"RunVerge",`), nil, 1) },
		"numeric_float":    func(b []byte) []byte { return bytes.Replace(b, []byte(`"mode":420`), []byte(`"mode":420.0`), 1) },
		"numeric_exponent": func(b []byte) []byte { return bytes.Replace(b, []byte(`"mode":420`), []byte(`"mode":42e1`), 1) },
		"trailing":         func(b []byte) []byte { return append(b, []byte(` {}`)...) },
		"truncated":        func(b []byte) []byte { return b[:len(b)-1] },
		"utf8":             func(b []byte) []byte { return bytes.Replace(b, []byte("RunVerge"), []byte{255}, 1) },
		"surrogate":        func(b []byte) []byte { return bytes.Replace(b, []byte("RunVerge"), []byte(`\ud800`), 1) },
		"oversized":        func(b []byte) []byte { return append(b, bytes.Repeat([]byte{' '}, maxManifest)...) },
	} {
		t.Run(name, func(t *testing.T) {
			f := independentBundle(t)
			raw, _ := json.Marshal(f.manifest)
			bad := change(raw)
			if bytes.Equal(raw, bad) {
				t.Fatal("negative fixture did not alter bytes")
			}
			hash := independentBundleWriteRaw(t, f.directory, bad)
			independentBundleRejected(t, f, hash)
		})
	}
}

func TestIndependentNativeBundleRejectsManifestPathAliasesAndBounds(t *testing.T) {
	paths := []string{"../outside", "/absolute", "./LICENSE", "web//index.html", "web/../LICENSE", "web\\index.html", "C:\\escape", "web/index.html/", "web/\x00x", ManifestName, MarkerName, ".hidden", "web/" + strings.Repeat("x", 129)}
	for _, name := range paths {
		t.Run(strings.ReplaceAll(name, "/", "_"), func(t *testing.T) {
			f := independentBundle(t)
			m := independentBundleClone(t, f.manifest)
			m.Files[0].Path = name
			sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
			hash := independentBundleWriteManifest(t, f.directory, m)
			independentBundleRejected(t, f, hash)
		})
	}
	for name, change := range map[string]func(*Manifest){
		"duplicate_file": func(m *Manifest) { m.Files = append(m.Files, m.Files[0]) },
		"case_file": func(m *Manifest) {
			item := m.Files[0]
			item.Path = strings.ToLower(item.Path)
			m.Files = append(m.Files, item)
		},
		"case_directory": func(m *Manifest) {
			m.Files = append(m.Files, File{Path: "Web/extra.js", SHA256: independentBundleHash(nil), Size: 0, Mode: 0644})
		},
		"file_parent": func(m *Manifest) {
			m.Files = append(m.Files, File{Path: "web", SHA256: independentBundleHash(nil), Size: 0, Mode: 0644})
		},
		"negative_size": func(m *Manifest) { m.Files[0].Size = -1 },
		"huge_file":     func(m *Manifest) { m.Files[0].Size = maxFile + 1 },
		"aggregate_size": func(m *Manifest) {
			for i := range m.Files {
				m.Files[i].Size = maxFile
			}
		},
		"wrong_mode":     func(m *Manifest) { m.Files[0].Mode = 0777 },
		"special_mode":   func(m *Manifest) { m.Files[0].Mode = 04755 },
		"uppercase_hash": func(m *Manifest) { m.Files[0].SHA256 = strings.ToUpper(m.Files[0].SHA256) },
		"missing_required": func(m *Manifest) {
			for i, f := range m.Files {
				if f.Path == "RUNNING.md" {
					m.Files = append(m.Files[:i], m.Files[i+1:]...)
					break
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := independentBundle(t)
			m := independentBundleClone(t, f.manifest)
			change(&m)
			sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
			hash := independentBundleWriteManifest(t, f.directory, m)
			independentBundleRejected(t, f, hash)
		})
	}
}

func TestIndependentNativeBundleIdentityIsExactAndOrdered(t *testing.T) {
	for name, change := range map[string]func(*Manifest){
		"api": func(m *Manifest) { m.Identity.APIVersion = "future" }, "os": func(m *Manifest) { m.Identity.OS = "other" }, "arch": func(m *Manifest) { m.Identity.Arch = "other" },
		"source":         func(m *Manifest) { m.Identity.SourceSHA256 = independentBundleHash([]byte("different source")) },
		"migration_hash": func(m *Manifest) { m.Identity.Migrations[0].SHA256 = independentBundleHash([]byte("rewritten SQL")) },
		"migration_order": func(m *Manifest) {
			m.Identity.Migrations[0], m.Identity.Migrations[1] = m.Identity.Migrations[1], m.Identity.Migrations[0]
		},
		"migration_extra": func(m *Manifest) {
			m.Identity.Migrations = append(m.Identity.Migrations, Migration{Name: "migrations/003_other.sql", SHA256: independentBundleHash(nil)})
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := independentBundle(t)
			m := independentBundleClone(t, f.manifest)
			change(&m)
			hash := independentBundleWriteManifest(t, f.directory, m)
			independentBundleRejected(t, f, hash)
		})
	}
}

func TestIndependentNativeBundleRejectsLinksFIFOsAndUnlistedInventory(t *testing.T) {
	for _, kind := range []string{"file_symlink", "directory_symlink", "dangling_symlink", "fifo", "extra_file", "extra_directory", "special_bits"} {
		t.Run(kind, func(t *testing.T) {
			f := independentBundle(t)
			switch kind {
			case "file_symlink":
				raw, _ := os.ReadFile(filepath.Join(f.directory, "LICENSE"))
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(outside, raw, 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(f.directory, "LICENSE")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(f.directory, "LICENSE")); err != nil {
					t.Fatal(err)
				}
			case "directory_symlink":
				old := filepath.Join(f.directory, "web")
				outside := filepath.Join(t.TempDir(), "moved-web")
				if err := os.Rename(old, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, old); err != nil {
					t.Fatal(err)
				}
			case "dangling_symlink":
				if err := os.Symlink("absent", filepath.Join(f.directory, "extra")); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := os.Remove(filepath.Join(f.directory, "LICENSE")); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(filepath.Join(f.directory, "LICENSE"), 0644); err != nil {
					t.Fatal(err)
				}
			case "extra_file":
				if err := os.WriteFile(filepath.Join(f.directory, "extra"), []byte("unlisted"), 0644); err != nil {
					t.Fatal(err)
				}
			case "extra_directory":
				if err := os.Mkdir(filepath.Join(f.directory, "extra"), 0755); err != nil {
					t.Fatal(err)
				}
			case "special_bits":
				name := filepath.Join(f.directory, "bin/platform")
				// This host strips setuid/setgid even when chmod succeeds. Sticky
				// survives, so assert its actual presence before testing rejection.
				if err := os.Chmod(name, 0755|os.ModeSticky); err != nil {
					t.Fatal(err)
				}
				if actual, err := os.Lstat(name); err != nil || actual.Mode()&os.ModeSticky == 0 {
					t.Fatal("special-bit fixture did not acquire the tested bit", err)
				}
				defer os.Chmod(name, 0755)
			}
			done := make(chan error, 1)
			go func() { _, err := Inspect(f.directory, f.hash, f.identity, false); done <- err }()
			select {
			case err := <-done:
				if !errors.Is(err, ErrInvalid) {
					t.Fatal("unsafe file tree accepted", err)
				}
			case <-time.After(time.Second):
				t.Fatal("special file inspection blocked")
			}
		})
	}
	f := independentBundle(t)
	link := filepath.Join(t.TempDir(), "source-link")
	if err := os.Symlink(f.directory, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(link, f.hash, f.identity, false); err == nil {
		t.Fatal("symlink bundle root accepted")
	}
}

func TestIndependentNativeBundleExistingDestinationsAndOverlapNeverOverwrite(t *testing.T) {
	f := independentBundle(t)
	for _, kind := range []string{"file", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			destination := filepath.Join(base, "existing")
			outside := filepath.Join(base, "sentinel")
			want := []byte("PRESERVE_EXISTING_CONTENT")
			if err := os.WriteFile(outside, want, 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "file":
				if err := os.WriteFile(destination, want, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(destination, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(outside, destination); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Install(f.directory, destination, f.hash, f.identity); !errors.Is(err, ErrDestination) {
				t.Fatal("existing destination accepted", err)
			}
			after, err := os.ReadFile(outside)
			if err != nil || !bytes.Equal(want, after) {
				t.Fatal("existing content overwritten", err)
			}
			if kind == "file" {
				after, err = os.ReadFile(destination)
				if err != nil || !bytes.Equal(want, after) {
					t.Fatal("existing destination changed", err)
				}
			}
		})
	}
	inside := filepath.Join(f.directory, "nested-install")
	if _, err := Install(f.directory, inside, f.hash, f.identity); !errors.Is(err, ErrDestination) {
		t.Fatal("source mutated by nested installation", err)
	}
	if _, err := os.Lstat(inside); !os.IsNotExist(err) {
		t.Fatal("overlap created a directory")
	}
	if _, err := Inspect(f.directory, f.hash, f.identity, false); err != nil {
		t.Fatal("failed installation changed source", err)
	}
}

type independentTruncateWriter struct {
	filename string
	once     sync.Once
}

func (w *independentTruncateWriter) Write(p []byte) (int, error) {
	var err error
	w.once.Do(func() { err = os.Truncate(w.filename, 0) })
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func TestIndependentNativeBundleTruncatedStreamingAndIncompleteCopyFailClosed(t *testing.T) {
	f := independentBundle(t)
	name := "large.bin"
	body := bytes.Repeat([]byte{42}, 2<<20)
	full := filepath.Join(f.directory, name)
	if err := os.WriteFile(full, body, 0644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(f.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	item := File{Path: name, SHA256: independentBundleHash(body), Size: int64(len(body)), Mode: 0644}
	if err = hashFile(root, item, &independentTruncateWriter{filename: full}); !errors.Is(err, ErrInvalid) {
		t.Fatal("truncated source passed streaming verification", err)
	}
	directory := t.TempDir()
	dst, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err = createFile(dst, "partial", 0644, func(w io.Writer) error {
		_, e := w.Write([]byte("partial bytes"))
		if e != nil {
			return e
		}
		return io.ErrUnexpectedEOF
	}); !errors.Is(err, ErrDestination) {
		t.Fatal("failed copy was reported complete", err)
	}
	raw, err := os.ReadFile(filepath.Join(directory, "partial"))
	if err != nil || string(raw) != "partial bytes" {
		t.Fatal("failure silently claimed rollback", err)
	}
	if _, err = os.Stat(filepath.Join(directory, MarkerName)); !os.IsNotExist(err) {
		t.Fatal("failed copy acquired completion marker")
	}
	if err = createFile(dst, "partial", 0644, func(w io.Writer) error { _, e := w.Write([]byte("overwrite")); return e }); !errors.Is(err, ErrDestination) {
		t.Fatal("partial file reused")
	}
}

func TestIndependentNativeBundleInstalledTamperingAndMarkerBinding(t *testing.T) {
	for _, kind := range []string{"payload", "manifest", "marker", "extra", "mode", "marker_duplicate"} {
		t.Run(kind, func(t *testing.T) {
			f := independentBundle(t)
			destination := filepath.Join(t.TempDir(), "installed")
			if _, err := Install(f.directory, destination, f.hash, f.identity); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "payload":
				file := filepath.Join(destination, "web/index.html")
				raw, _ := os.ReadFile(file)
				raw[0] ^= 1
				if err := os.WriteFile(file, raw, 0644); err != nil {
					t.Fatal(err)
				}
			case "manifest":
				raw, _ := os.ReadFile(filepath.Join(destination, ManifestName))
				if err := os.WriteFile(filepath.Join(destination, ManifestName), append(raw, ' '), 0644); err != nil {
					t.Fatal(err)
				}
			case "marker":
				raw, _ := json.Marshal(marker{Format: Format, ManifestSHA256: independentBundleHash([]byte("other manifest"))})
				if err := os.WriteFile(filepath.Join(destination, MarkerName), raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "marker_duplicate":
				raw := []byte(`{"format":"` + Format + `","manifest_sha256":"` + f.hash + `","manifest_sha256":"` + f.hash + `"}`)
				if err := os.WriteFile(filepath.Join(destination, MarkerName), raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "extra":
				if err := os.WriteFile(filepath.Join(destination, "credentials.env"), []byte("not a payload"), 0600); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(filepath.Join(destination, "bin/platform"), 0777); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Inspect(destination, f.hash, f.identity, true); !errors.Is(err, ErrInvalid) {
				t.Fatal("changed installation verified", err)
			}
		})
	}
}

func TestIndependentNativeBundleConcurrentInstallHasOneWinner(t *testing.T) {
	f := independentBundle(t)
	destination := filepath.Join(t.TempDir(), "one-install")
	errs := make(chan error, 4)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := Install(f.directory, destination, f.hash, f.identity); errs <- err }()
	}
	wg.Wait()
	close(errs)
	winners := 0
	for err := range errs {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrDestination) {
			t.Fatal("unexpected concurrent failure", err)
		}
	}
	if winners != 1 {
		t.Fatal("multiple or missing installation winners", winners)
	}
	if _, err := Inspect(destination, f.hash, f.identity, true); err != nil {
		t.Fatal("winning installation incomplete", err)
	}
}
