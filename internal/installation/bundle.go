// SPDX-License-Identifier: Elastic-2.0
// Package installation validates and copies an offline native application bundle.
// It never opens a database, runs packaged code, changes services or downloads files.
package installation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"agentplatform.local/platform/internal/jsonwire"
)

const Format = "runverge-native-bundle-v1"
const ManifestName = "bundle.json"
const MarkerName = "installation.json"
const maxManifest = 1 << 20
const maxFile = 256 << 20
const maxTotal = 1 << 30

var ErrInvalid = errors.New("invalid or changed native bundle; verify its manifest, files and permissions")
var ErrDestination = errors.New("installation requires a new directory in an existing trusted parent; incomplete directories are never reused")
var hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var componentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9][A-Za-z0-9.-]{0,63})?$`)

type Migration struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
}

type Identity struct {
	APIVersion   string      `json:"api_version"`
	OS           string      `json:"os"`
	Arch         string      `json:"arch"`
	SourceSHA256 string      `json:"source_sha256"`
	Migrations   []Migration `json:"migrations"`
}

type Manifest struct {
	Format   string   `json:"format"`
	Product  string   `json:"product"`
	Version  string   `json:"version"`
	Identity Identity `json:"identity"`
	Files    []File   `json:"files"`
}

type Report struct {
	ManifestSHA256         string   `json:"manifest_sha256"`
	Product                string   `json:"product"`
	Version                string   `json:"version"`
	Identity               Identity `json:"identity"`
	FileCount              int      `json:"file_count"`
	TotalBytes             int64    `json:"total_bytes"`
	Installed              bool     `json:"installed"`
	PublisherAuthenticated bool     `json:"publisher_authenticated"`
}

type marker struct {
	Format         string `json:"format"`
	ManifestSHA256 string `json:"manifest_sha256"`
}

func digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

// strictDecode requires exact case, complete fields, no duplicate keys, no null,
// bounded nesting and canonical numeric tokens. JSON formatting is immaterial.
func strictDecode(raw []byte, target any) error {
	if len(raw) == 0 || len(raw) > maxManifest || !jsonwire.ValidUnicode(raw) {
		return ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	value, err := readJSON(d, 0)
	if err != nil {
		return ErrInvalid
	}
	if _, err = d.Token(); err != io.EOF {
		return ErrInvalid
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(target); err != nil {
		return ErrInvalid
	}
	canonical, err := json.Marshal(target)
	if err != nil {
		return ErrInvalid
	}
	d = json.NewDecoder(bytes.NewReader(canonical))
	d.UseNumber()
	expected, err := readJSON(d, 0)
	if err != nil || !reflect.DeepEqual(value, expected) {
		return ErrInvalid
	}
	return nil
}

func readJSON(d *json.Decoder, depth int) (any, error) {
	if depth > 12 {
		return nil, ErrInvalid
	}
	token, err := d.Token()
	if err != nil || token == nil {
		return nil, ErrInvalid
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		value := map[string]any{}
		for d.More() {
			key, e := d.Token()
			name, ok := key.(string)
			if e != nil || !ok || len(value) > 8192 {
				return nil, ErrInvalid
			}
			if _, exists := value[name]; exists {
				return nil, ErrInvalid
			}
			item, e := readJSON(d, depth+1)
			if e != nil {
				return nil, e
			}
			value[name] = item
		}
		end, e := d.Token()
		if e != nil || end != json.Delim('}') {
			return nil, ErrInvalid
		}
		return value, nil
	case '[':
		value := []any{}
		for d.More() {
			if len(value) >= 8192 {
				return nil, ErrInvalid
			}
			item, e := readJSON(d, depth+1)
			if e != nil {
				return nil, e
			}
			value = append(value, item)
		}
		end, e := d.Token()
		if e != nil || end != json.Delim(']') {
			return nil, ErrInvalid
		}
		return value, nil
	default:
		return nil, ErrInvalid
	}
}

func validPath(name string) bool {
	if len(name) > 512 || name == ManifestName || name == MarkerName {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if !componentPattern.MatchString(part) || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func validate(m Manifest, expected Identity) error {
	if m.Format != Format || m.Product != "RunVerge" || !versionPattern.MatchString(m.Version) || len(m.Files) < 5 || len(m.Files) > 4096 {
		return ErrInvalid
	}
	i := m.Identity
	if i.APIVersion != expected.APIVersion || i.OS != expected.OS || i.Arch != expected.Arch || !hashPattern.MatchString(i.SourceSHA256) || !reflect.DeepEqual(i.Migrations, expected.Migrations) {
		return ErrInvalid
	}
	if expected.SourceSHA256 != "" && i.SourceSHA256 != expected.SourceSHA256 {
		return ErrInvalid
	}
	if len(i.Migrations) == 0 || len(i.Migrations) > 1024 {
		return ErrInvalid
	}
	for n, migration := range i.Migrations {
		if !strings.HasPrefix(migration.Name, "migrations/") || !strings.HasSuffix(migration.Name, ".sql") || !validPath(migration.Name) || !hashPattern.MatchString(migration.SHA256) || (n > 0 && i.Migrations[n-1].Name >= migration.Name) {
			return ErrInvalid
		}
	}
	seen := map[string]bool{}
	spellings := map[string]string{}
	total := int64(0)
	for n, item := range m.Files {
		if !validPath(item.Path) || !hashPattern.MatchString(item.SHA256) || item.Size < 0 || item.Size > maxFile || (n > 0 && m.Files[n-1].Path >= item.Path) {
			return ErrInvalid
		}
		// Case-folded uniqueness makes an artifact unambiguous on macOS volumes.
		key := strings.ToLower(item.Path)
		if seen[key] {
			return ErrInvalid
		}
		seen[key] = true
		for name := item.Path; name != "."; name = path.Dir(name) {
			folded := strings.ToLower(name)
			if prior, ok := spellings[folded]; ok && prior != name {
				return ErrInvalid
			}
			spellings[folded] = name
		}
		if item.Path == "bin/platform" {
			if item.Mode != 0755 || item.Size == 0 {
				return ErrInvalid
			}
		} else if item.Mode != 0644 {
			return ErrInvalid
		}
		total += item.Size
		if total > maxTotal {
			return ErrInvalid
		}
	}
	for _, item := range m.Files {
		for parent := path.Dir(item.Path); parent != "."; parent = path.Dir(parent) {
			if seen[strings.ToLower(parent)] {
				return ErrInvalid
			}
		}
	}
	for _, required := range []string{"bin/platform", "web/index.html", "LICENSE", "components.json", "source-files.json", "RUNNING.md"} {
		found := false
		for _, item := range m.Files {
			if item.Path == required {
				found = true
				break
			}
		}
		if !found {
			return ErrInvalid
		}
	}
	return nil
}

// openRegular also checks lstat/open identity and uses nonblocking opens so
// swapping a regular file for a FIFO cannot hang an installer. os.Root confines
// traversal; the independent lstat/identity checks reject symlinks and races.
func openRegular(root *os.Root, name string, size int64, mode fs.FileMode) (*os.File, error) {
	before, err := root.Lstat(name)
	if err != nil || before.Mode() != mode || before.Size() != size {
		return nil, ErrInvalid
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrInvalid
	}
	after, err := f.Stat()
	current, currentErr := root.Lstat(name)
	if err != nil || currentErr != nil || after.Mode() != mode || current.Mode() != mode || !os.SameFile(before, after) || !os.SameFile(after, current) || after.Size() != size {
		f.Close()
		return nil, ErrInvalid
	}
	return f, nil
}

func readBounded(root *os.Root, name string, limit int64, mode fs.FileMode) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil || info.Size() <= 0 || info.Size() > limit {
		return nil, ErrInvalid
	}
	f, err := openRegular(root, name, info.Size(), mode)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, info.Size()+1))
	if err != nil || int64(len(raw)) != info.Size() {
		return nil, ErrInvalid
	}
	return raw, nil
}

func openBundle(directory string) (*os.Root, error) {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Mode().Perm()&0022 != 0 {
		return nil, ErrInvalid
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, ErrInvalid
	}
	return root, nil
}

func load(root *os.Root, expectedHash string, expected Identity, installed bool) (Manifest, []byte, error) {
	var m Manifest
	raw, err := readBounded(root, ManifestName, maxManifest, 0644)
	if err != nil || (expectedHash != "" && (!hashPattern.MatchString(expectedHash) || digest(raw) != expectedHash)) {
		return m, nil, ErrInvalid
	}
	if strictDecode(raw, &m) != nil || validate(m, expected) != nil {
		return m, nil, ErrInvalid
	}
	if installed {
		markerRaw, err := readBounded(root, MarkerName, 4096, 0600)
		var mark marker
		if err != nil || strictDecode(markerRaw, &mark) != nil || mark.Format != Format || mark.ManifestSHA256 != digest(raw) {
			return m, nil, ErrInvalid
		}
	}
	allowed := map[string]bool{ManifestName: true}
	directories := map[string]bool{".": true}
	if installed {
		allowed[MarkerName] = true
	}
	for _, item := range m.Files {
		allowed[item.Path] = true
		for dir := path.Dir(item.Path); dir != "."; dir = path.Dir(dir) {
			directories[dir] = true
		}
	}
	count := 0
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return ErrInvalid
		}
		count++
		if count > 8192 {
			return ErrInvalid
		}
		info, e := root.Lstat(name)
		if e != nil {
			return ErrInvalid
		}
		if info.IsDir() {
			if !directories[name] || info.Mode().Perm()&0022 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
				return ErrInvalid
			}
			return nil
		}
		if !info.Mode().IsRegular() || !allowed[name] {
			return ErrInvalid
		}
		return nil
	})
	if err != nil {
		return m, nil, ErrInvalid
	}
	return m, raw, nil
}

func hashFile(root *os.Root, item File, output io.Writer) error {
	f, err := openRegular(root, item.Path, item.Size, fs.FileMode(item.Mode))
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	writer := io.Writer(h)
	if output != nil {
		writer = io.MultiWriter(h, output)
	}
	n, err := io.Copy(writer, io.LimitReader(f, item.Size+1))
	if err != nil || n != item.Size || hex.EncodeToString(h.Sum(nil)) != item.SHA256 {
		return ErrInvalid
	}
	after, err := root.Lstat(item.Path)
	current, e := f.Stat()
	if err != nil || e != nil || after.Mode() != fs.FileMode(item.Mode) || current.Mode() != fs.FileMode(item.Mode) || !os.SameFile(after, current) || after.Size() != item.Size {
		return ErrInvalid
	}
	return nil
}

func report(m Manifest, raw []byte, installed bool) Report {
	total := int64(0)
	for _, item := range m.Files {
		total += item.Size
	}
	return Report{ManifestSHA256: digest(raw), Product: m.Product, Version: m.Version, Identity: m.Identity, FileCount: len(m.Files), TotalBytes: total, Installed: installed, PublisherAuthenticated: false}
}

// Inspect checks content integrity against an optional separately obtained hash.
// It does not authenticate the publisher and never executes the bundled binary.
func Inspect(directory, expectedHash string, expected Identity, installed bool) (Report, error) {
	root, err := openBundle(directory)
	if err != nil {
		return Report{}, err
	}
	defer root.Close()
	m, raw, err := load(root, expectedHash, expected, installed)
	if err != nil {
		return Report{}, err
	}
	for _, item := range m.Files {
		if hashFile(root, item, nil) != nil {
			return Report{}, ErrInvalid
		}
	}
	// Detect inventory/manifest changes that occurred while hashing payloads.
	_, again, err := load(root, expectedHash, expected, installed)
	if err != nil || !bytes.Equal(raw, again) {
		return Report{}, ErrInvalid
	}
	return report(m, raw, installed), nil
}

func createFile(root *os.Root, name string, mode fs.FileMode, write func(io.Writer) error) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return ErrDestination
	}
	err = write(f)
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrDestination
	}
	return nil
}

// Install reserves a new directory and writes its completion marker last. Failure
// leaves an incomplete directory for explicit inspection/removal; it never reuses
// or removes an existing installation. Runtime data belongs outside this tree.
func Install(source, destination, expectedHash string, expected Identity) (Report, error) {
	if !hashPattern.MatchString(expectedHash) {
		return Report{}, ErrInvalid
	}
	src, err := openBundle(source)
	if err != nil {
		return Report{}, err
	}
	defer src.Close()
	m, raw, err := load(src, expectedHash, expected, false)
	if err != nil {
		return Report{}, err
	}
	for _, item := range m.Files {
		if hashFile(src, item, nil) != nil {
			return Report{}, ErrInvalid
		}
	}
	clean, err := filepath.Abs(destination)
	if err != nil || clean == string(filepath.Separator) {
		return Report{}, ErrDestination
	}
	resolvedSource, sourceErr := filepath.EvalSymlinks(source)
	resolvedParent, parentErr := filepath.EvalSymlinks(filepath.Dir(clean))
	if sourceErr != nil || parentErr != nil {
		return Report{}, ErrDestination
	}
	resolvedSource, sourceErr = filepath.Abs(resolvedSource)
	resolvedParent, parentErr = filepath.Abs(resolvedParent)
	if sourceErr != nil || parentErr != nil {
		return Report{}, ErrDestination
	}
	resolvedDestination := filepath.Join(resolvedParent, filepath.Base(clean))
	if resolvedDestination == resolvedSource || strings.HasPrefix(resolvedDestination, resolvedSource+string(filepath.Separator)) || strings.HasPrefix(resolvedSource, resolvedDestination+string(filepath.Separator)) {
		return Report{}, ErrDestination
	}
	parent, err := openBundle(filepath.Dir(clean))
	if err != nil {
		return Report{}, ErrDestination
	}
	defer parent.Close()
	base := filepath.Base(clean)
	if !componentPattern.MatchString(base) {
		return Report{}, ErrDestination
	}
	if err = parent.Mkdir(base, 0700); err != nil {
		return Report{}, ErrDestination
	}
	dst, err := parent.OpenRoot(base)
	if err != nil {
		return Report{}, ErrDestination
	}
	defer dst.Close()
	dirs := map[string]bool{}
	for _, item := range m.Files {
		for dir := path.Dir(item.Path); dir != "."; dir = path.Dir(dir) {
			dirs[dir] = true
		}
	}
	ordered := make([]string, 0, len(dirs))
	for dir := range dirs {
		ordered = append(ordered, dir)
	}
	sort.Strings(ordered)
	for _, dir := range ordered {
		if dst.Mkdir(dir, 0755) != nil {
			return Report{}, ErrDestination
		}
	}
	for _, item := range m.Files {
		if createFile(dst, item.Path, fs.FileMode(item.Mode), func(out io.Writer) error { return hashFile(src, item, out) }) != nil {
			return Report{}, ErrDestination
		}
	}
	if createFile(dst, ManifestName, 0644, func(out io.Writer) error { _, e := out.Write(raw); return e }) != nil {
		return Report{}, ErrDestination
	}
	if _, err = Inspect(clean, expectedHash, expected, false); err != nil {
		return Report{}, err
	}
	completion, _ := json.Marshal(marker{Format: Format, ManifestSHA256: expectedHash})
	if createFile(dst, MarkerName, 0600, func(out io.Writer) error { _, e := out.Write(append(completion, '\n')); return e }) != nil {
		return Report{}, ErrDestination
	}
	// Sync directory entries before reporting success on supported native systems.
	for _, dir := range append(ordered, ".") {
		f, e := dst.Open(dir)
		if e != nil {
			return Report{}, ErrDestination
		}
		e = f.Sync()
		_ = f.Close()
		if e != nil {
			return Report{}, ErrDestination
		}
	}
	parentFile, err := parent.Open(".")
	if err != nil {
		return Report{}, ErrDestination
	}
	err = parentFile.Sync()
	_ = parentFile.Close()
	if err != nil {
		return Report{}, ErrDestination
	}
	return Inspect(clean, expectedHash, expected, true)
}
