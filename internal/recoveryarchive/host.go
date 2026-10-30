// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
)

var ErrHostFiles = errors.New("private recovery files unavailable; inspect the exact output before another attempt")
var ErrPublication = errors.New("backup publication not confirmed; inspect the private output, do not overwrite or retry automatically")

// HostKeys is a single-owner private archive key bundle. It is deliberately
// separate from recovery-access keys. This first profile does not implement
// operational separation of source signing and target decryption key custody.
type hostKeys struct {
	Version     int    `json:"version"`
	Identity    string `json:"identity"`
	SigningSeed []byte `json:"signing_seed"`
}
type HostKeySummary struct {
	Recipient        string `json:"recipient"`
	SigningKeySHA256 string `json:"signing_key_sha256"`
}
type HostBackupReceipt struct {
	Manifest      Manifest `json:"manifest"`
	ReceiptSHA256 string   `json:"receipt_sha256"`
}

// WriteHostJSON persists a new private host intent. The caller must do this
// before a related database mutation; existing/partial files are never adopted.
func WriteHostJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return ErrHostFiles
	}
	return writeHostPrivateFile(path, raw)
}

// Database credentials/target intent must not enter the transportable backup.
func WriteHostJSONOutside(directory, path string, value any) error {
	if !hostPathOutside(directory, path) {
		return ErrOptions
	}
	return WriteHostJSON(path, value)
}

func GenerateHostKeys(path string) (HostKeySummary, error) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return HostKeySummary{}, ErrOptions
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return HostKeySummary{}, ErrOptions
	}
	defer clear(key)
	keys := hostKeys{Version: 1, Identity: identity.String(), SigningSeed: append([]byte(nil), key.Seed()...)}
	defer clear(keys.SigningSeed)
	raw, err := json.Marshal(keys)
	if err != nil {
		return HostKeySummary{}, ErrOptions
	}
	defer clear(raw)
	if err = writeHostPrivateFile(path, raw); err != nil {
		return HostKeySummary{}, err
	}
	return HostKeySummary{Recipient: identity.Recipient().String(), SigningKeySHA256: digest(pub)}, nil
}

func loadHostKeys(path string) (*age.X25519Identity, ed25519.PrivateKey, error) {
	raw, err := readHostPrivateFile(path, 4096)
	if err != nil {
		return nil, nil, ErrHostFiles
	}
	defer clear(raw)
	var keys hostKeys
	if json.Unmarshal(raw, &keys) != nil {
		return nil, nil, ErrOptions
	}
	defer clear(keys.SigningSeed)
	canonical, _ := json.Marshal(keys)
	defer clear(canonical)
	if !bytes.Equal(raw, canonical) || keys.Version != 1 || len(keys.SigningSeed) != ed25519.SeedSize {
		return nil, nil, ErrOptions
	}
	identity, err := age.ParseX25519Identity(keys.Identity)
	if err != nil {
		return nil, nil, ErrOptions
	}
	return identity, ed25519.NewKeyFromSeed(keys.SigningSeed), nil
}

// HostLimits bounds the default command profile, not the entire machine's disk
// quota. Creation can stage plaintext plus ciphertext while publishing a second
// ciphertext copy. It does not compress or extract arbitrary paths.
func HostLimits() Limits { return Limits{MaxPlaintextBytes: 384 << 20, MaxCiphertextBytes: 400 << 20} }

// CreateHostBackup reserves a new directory, spools a bounded trusted export to
// an anonymous private file, then writes an encrypted archive and exact receipt.
// The callback supplies data and metadata only, never SQL or filesystem paths.
// Errors retain the output directory; incomplete output is not a backup receipt.
func CreateHostBackup(ctx context.Context, directory, keysFile, stagingParent string, export func(context.Context, io.Writer) (Metadata, error)) (result HostBackupReceipt, resultErr error) {
	if ctx == nil || export == nil {
		return result, ErrOptions
	}
	identity, key, err := loadHostKeys(keysFile)
	if err != nil {
		return result, err
	}
	defer clear(key)
	bundle, err := newHostBundle(ctx, directory)
	if err != nil {
		return result, err
	}
	defer bundle.close()
	stage, err := newStaging(stagingParent)
	if err != nil {
		return result, err
	}
	defer func() {
		if stage.Close() != nil {
			result, resultErr = HostBackupReceipt{}, ErrPublication
		}
	}()
	plain, err := stage.anonymousFile("plaintext")
	if err != nil {
		return result, err
	}
	limits := HostLimits()
	sink := &boundedSink{dst: plain, h: sha256.New(), max: limits.MaxPlaintextBytes}
	metadata, err := export(ctx, sink)
	if err != nil {
		return result, err
	}
	if !metadata.valid() || plain.Sync() != nil {
		return result, ErrIO
	}
	if _, err = plain.Seek(0, io.SeekStart); err != nil {
		return result, ErrIO
	}
	file, err := bundle.create("archive.rvg")
	if err != nil {
		return result, ErrHostFiles
	}
	out := &boundedSink{dst: file, h: sha256.New(), max: limits.MaxCiphertextBytes + MaxManifestBytes + int64(len(Magic)) + 4 + ed25519.SignatureSize}
	manifest, sealErr := Seal(ctx, out, plain, SealOptions{Recipient: identity.Recipient(), SigningKey: key, Metadata: metadata, Limits: limits, StagingParent: stagingParent})
	syncErr := file.Sync()
	closeErr := file.Close()
	if sealErr != nil {
		return result, sealErr
	}
	if syncErr != nil || closeErr != nil || manifest.PlaintextBytes != sink.n || manifest.PlaintextSHA256 != sink.sum() {
		return result, ErrPublication
	}
	raw, err := json.Marshal(manifest)
	if err != nil || len(raw) > MaxManifestBytes {
		return result, ErrPublication
	}
	if err = bundle.write("receipt.json", raw); err != nil {
		return result, ErrPublication
	}
	if err = bundle.sync(); err != nil {
		return result, ErrPublication
	}
	return HostBackupReceipt{Manifest: manifest, ReceiptSHA256: digest(raw)}, nil
}

// VerifyHostBackup requires the independently selected receipt digest, never a
// digest learned automatically from the transported sidecar. Keys must come from
// outside the bundle. Authentication is not typed PostgreSQL import validation.
func VerifyHostBackup(ctx context.Context, directory, keysFile, expectedReceiptSHA256, stagingParent string) (archive *VerifiedArchive, resultErr error) {
	if !digestPattern.MatchString(expectedReceiptSHA256) || !hostPathOutside(directory, keysFile) {
		return nil, ErrOptions
	}
	identity, key, err := loadHostKeys(keysFile)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	bundle, err := openHostBundle(ctx, directory)
	if err != nil {
		return nil, err
	}
	defer bundle.close()
	raw, err := bundle.read("receipt.json", MaxManifestBytes)
	if err != nil || digest(raw) != expectedReceiptSHA256 {
		return nil, ErrInvalid
	}
	manifest, err := readManifest(raw, HostLimits())
	if err != nil {
		return nil, err
	}
	file, err := bundle.open("archive.rvg")
	if err != nil {
		return nil, err
	}
	defer func() {
		if file.Close() != nil {
			if archive != nil {
				_ = archive.Close()
			}
			archive, resultErr = nil, ErrHostFiles
		}
	}()
	info, err := file.Stat()
	if err != nil || info.Size() > HostLimits().MaxCiphertextBytes+MaxManifestBytes+int64(len(Magic))+4+ed25519.SignatureSize {
		return nil, ErrLimit
	}
	archive, err = Open(ctx, file, OpenOptions{Identity: identity, TrustedSigner: key.Public().(ed25519.PublicKey), Expected: manifest.Metadata, Limits: HostLimits(), StagingParent: stagingParent})
	if err != nil {
		return nil, err
	}
	if archive.Manifest() != manifest {
		_ = archive.Close()
		return nil, ErrInvalid
	}
	return archive, nil
}

// Ensure key custody does not come from the transported bundle, including
// canonical aliases. Protected ancestor namespace/local FS remain host duties.
func hostPathOutside(directory, path string) bool {
	root, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return false
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, parent)
	if err != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return false
	}
	info, err := os.Stat(root)
	if err != nil {
		return false
	}
	for ; ; parent = filepath.Dir(parent) {
		other, err := os.Stat(parent)
		if err != nil || os.SameFile(info, other) {
			return false
		}
		if filepath.Dir(parent) == parent {
			break
		}
	}
	return true
}
