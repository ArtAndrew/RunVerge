// SPDX-License-Identifier: Elastic-2.0
package installation

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
)

func authorBundle(t *testing.T) (string, Manifest, Identity, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bundle")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	identity := Identity{APIVersion: "0.14.0", OS: runtime.GOOS, Arch: runtime.GOARCH, SourceSHA256: strings.Repeat("a", 64), Migrations: []Migration{{Name: "migrations/001_test.sql", SHA256: strings.Repeat("b", 64)}}}
	m := Manifest{Format: Format, Product: "RunVerge", Version: "0.14.0-dev.test", Identity: identity}
	for name, raw := range map[string]string{"bin/platform": "this payload is never executed", "web/index.html": "<title>RunVerge</title>", "LICENSE": "test license", "components.json": "{}", "source-files.json": "{}", "RUNNING.md": "test instructions", "web/assets/app.js": "console.log('test');"} {
		mode := uint32(0644)
		if name == "bin/platform" {
			mode = 0755
		}
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(raw), os.FileMode(mode)); err != nil {
			t.Fatal(err)
		}
		m.Files = append(m.Files, File{Path: name, SHA256: digest([]byte(raw)), Size: int64(len(raw)), Mode: mode})
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	hash := authorManifest(t, dir, m)
	return dir, m, identity, hash
}

func authorManifest(t *testing.T, dir string, m Manifest) string {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, ManifestName), raw, 0644); err != nil {
		t.Fatal(err)
	}
	return digest(raw)
}

func TestNativeBundleAuthorInstallsAndVerifiesWithoutExecutingPayload(t *testing.T) {
	source, m, identity, hash := authorBundle(t)
	before, err := Inspect(source, hash, identity, false)
	if err != nil {
		t.Fatal(err)
	}
	if before.Installed || before.PublisherAuthenticated || before.FileCount != len(m.Files) {
		t.Fatalf("incorrect report: %+v", before)
	}
	destination := filepath.Join(t.TempDir(), "installed")
	after, err := Install(source, destination, hash, identity)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Installed || after.ManifestSHA256 != before.ManifestSHA256 {
		t.Fatal("installation identity changed")
	}
	for _, file := range m.Files {
		expected, _ := os.ReadFile(filepath.Join(source, file.Path))
		actual, _ := os.ReadFile(filepath.Join(destination, file.Path))
		if !bytes.Equal(expected, actual) {
			t.Fatal("payload differs")
		}
	}
	if _, err = Install(source, destination, hash, identity); err == nil {
		t.Fatal("existing installation accepted")
	}
	if _, err = Inspect(source, hash, identity, true); err == nil {
		t.Fatal("source claimed installed")
	}
	if _, err = Inspect(destination, hash, identity, false); err == nil {
		t.Fatal("installation marker ignored in source inventory")
	}
	if _, err = Inspect(destination, hash, identity, true); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(destination, "web/index.html"), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = Inspect(destination, hash, identity, true); err == nil {
		t.Fatal("changed installed web accepted")
	}
}

func TestNativeBundleAuthorRejectsWireAmbiguityBeforeDestinationCreation(t *testing.T) {
	for _, kind := range []string{"duplicate", "case-alias", "unknown", "missing", "null", "wrong-hash", "wrong-os", "wrong-schema", "wrong-source", "path-traversal", "case-directory-alias", "file-parent-conflict", "reserved-file", "unsorted"} {
		t.Run(kind, func(t *testing.T) {
			source, m, identity, hash := authorBundle(t)
			raw, _ := os.ReadFile(filepath.Join(source, ManifestName))
			switch kind {
			case "duplicate":
				raw = bytes.Replace(raw, []byte(`"format":`), []byte(`"format":"ignored","format":`), 1)
			case "case-alias":
				raw = bytes.Replace(raw, []byte(`"format":`), []byte(`"Format":`), 1)
			case "unknown":
				raw = append([]byte(`{"extra":true,`), raw[1:]...)
			case "missing":
				var obj map[string]json.RawMessage
				_ = json.Unmarshal(raw, &obj)
				delete(obj, "version")
				raw, _ = json.Marshal(obj)
			case "null":
				raw = bytes.Replace(raw, []byte(`"RunVerge"`), []byte(`null`), 1)
			case "wrong-hash":
				hash = strings.Repeat("c", 64)
			case "wrong-os":
				identity.OS = "unsupported"
			case "wrong-schema":
				identity.Migrations = []Migration{{Name: "migrations/001_test.sql", SHA256: strings.Repeat("d", 64)}}
			case "wrong-source":
				identity.SourceSHA256 = strings.Repeat("e", 64)
			case "path-traversal":
				m.Files[0].Path = "../escape"
				raw, _ = json.Marshal(m)
			case "case-directory-alias":
				m.Files = append(m.Files, File{Path: "Web/another.html", SHA256: strings.Repeat("f", 64), Size: 1, Mode: 0644})
				sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
				raw, _ = json.Marshal(m)
			case "file-parent-conflict":
				m.Files = append(m.Files, File{Path: "web", SHA256: strings.Repeat("f", 64), Size: 1, Mode: 0644})
				sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
				raw, _ = json.Marshal(m)
			case "reserved-file":
				m.Files[0].Path = ManifestName
				raw, _ = json.Marshal(m)
			case "unsorted":
				m.Files[0], m.Files[1] = m.Files[1], m.Files[0]
				raw, _ = json.Marshal(m)
			}
			if err := os.WriteFile(filepath.Join(source, ManifestName), raw, 0644); err != nil {
				t.Fatal(err)
			}
			if kind != "wrong-hash" {
				hash = digest(raw)
			}
			dest := filepath.Join(t.TempDir(), "rejected")
			if _, err := Install(source, dest, hash, identity); err == nil {
				t.Fatal("unsafe bundle accepted")
			}
			if _, err := os.Lstat(dest); !os.IsNotExist(err) {
				t.Fatal("invalid bundle created destination")
			}
		})
	}
}

func TestNativeBundleAuthorRejectsLinksSpecialFilesAndUnlistedContent(t *testing.T) {
	for _, kind := range []string{"file-link", "directory-link", "root-link", "fifo", "world-writable-file", "world-writable-directory", "extra-file", "extra-empty-directory", "oversized", "truncated"} {
		t.Run(kind, func(t *testing.T) {
			source, _, identity, hash := authorBundle(t)
			p := filepath.Join(source, "web/index.html")
			switch kind {
			case "file-link":
				original := filepath.Join(t.TempDir(), "original")
				_ = os.Rename(p, original)
				_ = os.Symlink(original, p)
			case "directory-link":
				original := filepath.Join(t.TempDir(), "web")
				_ = os.Rename(filepath.Join(source, "web"), original)
				_ = os.Symlink(original, filepath.Join(source, "web"))
			case "root-link":
				link := filepath.Join(t.TempDir(), "linked")
				_ = os.Symlink(source, link)
				source = link
			case "fifo":
				_ = os.Remove(p)
				if err := syscall.Mkfifo(p, 0644); err != nil {
					t.Fatal(err)
				}
			case "world-writable-file":
				_ = os.Chmod(p, 0666)
			case "world-writable-directory":
				_ = os.Chmod(filepath.Join(source, "web"), 0777)
			case "extra-file":
				_ = os.WriteFile(filepath.Join(source, "private.env"), []byte("not a real secret"), 0600)
			case "extra-empty-directory":
				_ = os.Mkdir(filepath.Join(source, "unlisted"), 0755)
			case "oversized":
				_ = os.WriteFile(p, bytes.Repeat([]byte("x"), 100), 0644)
			case "truncated":
				_ = os.WriteFile(p, []byte("x"), 0644)
			}
			if _, err := Inspect(source, hash, identity, false); err == nil {
				t.Fatal("invalid filesystem accepted")
			}
		})
	}
}

func TestNativeBundleAuthorMarkerAndDestinationBoundaries(t *testing.T) {
	source, _, identity, hash := authorBundle(t)
	if _, err := Install(source, filepath.Join(source, "nested"), hash, identity); err == nil {
		t.Fatal("source mutated by nested install")
	}
	if _, err := os.Lstat(filepath.Join(source, "nested")); !os.IsNotExist(err) {
		t.Fatal("nested destination created")
	}
	if _, err := Install(source, filepath.Join(t.TempDir(), "untrusted"), "", identity); err == nil {
		t.Fatal("missing hash accepted")
	}
	parent := t.TempDir()
	_ = os.Chmod(parent, 0777)
	if _, err := Install(source, filepath.Join(parent, "new"), hash, identity); err == nil {
		t.Fatal("writable parent accepted")
	}
	dest := filepath.Join(t.TempDir(), "installed")
	if _, err := Install(source, dest, hash, identity); err != nil {
		t.Fatal(err)
	}
	mark := filepath.Join(dest, MarkerName)
	_ = os.WriteFile(mark, []byte(`{"format":"runverge-native-bundle-v1","manifest_sha256":"`+strings.Repeat("0", 64)+`"}`), 0600)
	if _, err := Inspect(dest, hash, identity, true); err == nil {
		t.Fatal("mismatched marker accepted")
	}
	_ = os.Remove(mark)
	if _, err := Inspect(dest, hash, identity, true); err == nil {
		t.Fatal("incomplete installation accepted")
	}
}
