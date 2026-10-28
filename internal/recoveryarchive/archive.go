// SPDX-License-Identifier: Elastic-2.0
// Package recoveryarchive authenticates and encrypts one opaque data archive.
// It does not interpret tables, paths or SQL, import data, or authorize recovery.
// Open exposes plaintext only after the complete envelope and age stream pass.
package recoveryarchive

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"os"
	"regexp"
	"sync"
	"time"

	"filippo.io/age"
)

const Format = "runverge-recovery-age-x25519-ed25519-v1"
const Magic = "RUNVERGE-RECOVERY-1\n"
const SignatureDomain = "RunVerge recovery archive signature v1\x00"
const MaxManifestBytes = 4096

// MaxStagedBytes is a hard combined ceiling, not a default allocation or quota.
// Each caller must select smaller positive limits appropriate for its dataset.
const MaxStagedBytes int64 = 1 << 40

var (
	ErrInvalid    = errors.New("invalid or unauthenticated recovery archive")
	ErrOptions    = errors.New("invalid recovery archive options")
	ErrLimit      = errors.New("recovery archive exceeds configured limits")
	ErrIO         = errors.New("recovery archive input or output failed")
	ErrStaging    = errors.New("private recovery staging failed")
	ErrCanceled   = errors.New("recovery archive operation canceled")
	ErrClosed     = errors.New("verified recovery archive is closed")
	identifier    = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}_[a-f0-9]{32}$`)
	digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Metadata identifies the selected snapshot. Open requires an independent exact
// Expected value: a correctly signed stale or different source is not accepted.
// The signature attests to these claims; it does not prove source fencing or
// database snapshot consistency. Those are separate importer/operator duties.
type Metadata struct {
	BackupID     string `json:"backup_id"`
	SourceID     string `json:"source_id"`
	CutUTC       string `json:"cut_utc"`
	SchemaSHA256 string `json:"schema_sha256"`
}

type Manifest struct {
	Format           string   `json:"format"`
	Metadata         Metadata `json:"metadata"`
	SigningKeySHA256 string   `json:"signing_key_sha256"`
	CiphertextBytes  int64    `json:"ciphertext_bytes"`
	CiphertextSHA256 string   `json:"ciphertext_sha256"`
	PlaintextBytes   int64    `json:"plaintext_bytes"`
	PlaintextSHA256  string   `json:"plaintext_sha256"`
}

type Limits struct {
	MaxPlaintextBytes  int64
	MaxCiphertextBytes int64
}

type SealOptions struct {
	Recipient     *age.X25519Recipient
	SigningKey    ed25519.PrivateKey
	Metadata      Metadata
	Limits        Limits
	StagingParent string
}

type OpenOptions struct {
	Identity      *age.X25519Identity
	TrustedSigner ed25519.PublicKey
	Expected      Metadata
	Limits        Limits
	StagingParent string
}

func (m Metadata) valid() bool {
	if !identifier.MatchString(m.BackupID) || !identifier.MatchString(m.SourceID) || !digestPattern.MatchString(m.SchemaSHA256) {
		return false
	}
	cut, err := time.Parse(time.RFC3339Nano, m.CutUTC)
	return err == nil && cut.Year() >= 2000 && cut.Year() <= 9999 && cut.UTC().Format(time.RFC3339Nano) == m.CutUTC
}

func (l Limits) valid() bool {
	return l.MaxPlaintextBytes > 0 && l.MaxCiphertextBytes > 0 &&
		l.MaxPlaintextBytes <= MaxStagedBytes && l.MaxCiphertextBytes <= MaxStagedBytes-l.MaxPlaintextBytes
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func signatureMessage(raw []byte) []byte {
	result := make([]byte, 0, len(SignatureDomain)+4+len(raw))
	result = append(result, SignatureDomain...)
	result = binary.BigEndian.AppendUint32(result, uint32(len(raw)))
	return append(result, raw...)
}

func readManifest(raw []byte, limit Limits) (Manifest, error) {
	var m Manifest
	if len(raw) == 0 || len(raw) > MaxManifestBytes {
		return m, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&m) != nil {
		return Manifest{}, ErrInvalid
	}
	// Exact canonical bytes reject duplicate/unknown/case-folded fields, null,
	// trailing whitespace/JSON, noncanonical integers and escaped aliases.
	again, err := json.Marshal(m)
	if err != nil || !bytes.Equal(raw, again) || m.Format != Format || !m.Metadata.valid() ||
		!digestPattern.MatchString(m.SigningKeySHA256) || !digestPattern.MatchString(m.CiphertextSHA256) || !digestPattern.MatchString(m.PlaintextSHA256) ||
		m.CiphertextBytes <= 0 || m.PlaintextBytes < 0 {
		return Manifest{}, ErrInvalid
	}
	if m.CiphertextBytes > limit.MaxCiphertextBytes || m.PlaintextBytes > limit.MaxPlaintextBytes {
		return Manifest{}, ErrLimit
	}
	return m, nil
}

type boundedSink struct {
	dst io.Writer
	h   hash.Hash
	max int64
	n   int64
}

func (s *boundedSink) Write(p []byte) (int, error) {
	if int64(len(p)) > s.max-s.n {
		return 0, ErrLimit
	}
	n, err := s.dst.Write(p)
	if n > 0 && n <= len(p) {
		s.n += int64(n)
		_, _ = s.h.Write(p[:n])
	}
	if err != nil || n != len(p) {
		return n, ErrIO
	}
	return n, nil
}

func (s *boundedSink) sum() string { return hex.EncodeToString(s.h.Sum(nil)) }

func canceled(ctx context.Context) error {
	if ctx.Err() != nil {
		return ErrCanceled
	}
	return nil
}

type guardedReader struct {
	ctx        context.Context
	src        io.Reader
	emptyReads int
}

func (r *guardedReader) Read(p []byte) (int, error) {
	if err := canceled(r.ctx); err != nil {
		return 0, err
	}
	n, err := r.src.Read(p)
	if e := canceled(r.ctx); e != nil {
		return 0, e
	}
	if n < 0 || n > len(p) {
		return 0, ErrIO
	}
	if n == 0 && err == nil && len(p) > 0 {
		r.emptyReads++
		if r.emptyReads >= 100 {
			return 0, ErrIO
		}
	} else {
		r.emptyReads = 0
	}
	return n, err
}

func readHeader(ctx context.Context, src io.Reader, dst []byte) error {
	if _, err := io.ReadFull(src, dst); err != nil {
		if e := canceled(ctx); e != nil {
			return e
		}
		return ErrInvalid
	}
	return nil
}

// copyBounded checks context between I/O calls and rejects a reader that makes
// no progress repeatedly. The caller owns transport deadlines: a generic
// blocked io.Reader cannot be interrupted safely by this package.
func copyBounded(ctx context.Context, dst io.Writer, src io.Reader, max int64) (int64, error) {
	buffer := make([]byte, 64*1024)
	var total int64
	emptyReads := 0
	for {
		if err := canceled(ctx); err != nil {
			return total, err
		}
		want := int64(len(buffer))
		if remaining := max - total + 1; remaining < want {
			want = remaining
		}
		n, err := src.Read(buffer[:int(want)])
		if e := canceled(ctx); e != nil {
			return total, e
		}
		if n < 0 || n > int(want) {
			return total, ErrIO
		}
		if n > 0 {
			emptyReads = 0
			if int64(n) > max-total {
				return total, ErrLimit
			}
			if e := canceled(ctx); e != nil {
				return total, e
			}
			written, writeErr := dst.Write(buffer[:n])
			if writeErr != nil || written != n {
				if errors.Is(writeErr, ErrLimit) {
					return total, ErrLimit
				}
				return total, ErrIO
			}
			total += int64(n)
		} else if err == nil {
			emptyReads++
			if emptyReads >= 100 {
				return total, ErrIO
			}
		}
		if err == io.EOF {
			return total, canceled(ctx)
		}
		if err != nil {
			return total, ErrIO
		}
	}
}

func writeAll(ctx context.Context, dst io.Writer, raw []byte) error {
	if err := canceled(ctx); err != nil {
		return err
	}
	if n, err := dst.Write(raw); err != nil || n != len(raw) {
		return ErrIO
	}
	return nil
}

// Seal stages only ciphertext, then emits a signed envelope. Input errors or
// limits produce no output. Output errors may leave a partial envelope which
// Open rejects. The caller owns src/dst and must handle their Close/Sync errors
// and durable publication; successful Seal is not a completed backup receipt.
func Seal(ctx context.Context, dst io.Writer, plaintext io.Reader, options SealOptions) (result Manifest, resultErr error) {
	if ctx == nil || dst == nil || plaintext == nil || options.Recipient == nil || !options.Limits.valid() || !options.Metadata.valid() || len(options.SigningKey) != ed25519.PrivateKeySize {
		return Manifest{}, ErrOptions
	}
	key := append(ed25519.PrivateKey(nil), options.SigningKey...)
	defer clear(key)
	if !bytes.Equal(key, ed25519.NewKeyFromSeed(key[:ed25519.SeedSize])) {
		return Manifest{}, ErrOptions
	}
	if err := canceled(ctx); err != nil {
		return Manifest{}, err
	}
	stage, err := newStaging(options.StagingParent)
	if err != nil {
		return Manifest{}, err
	}
	defer func() {
		if stage.Close() != nil {
			result, resultErr = Manifest{}, ErrStaging
		}
	}()
	ciphertext, err := stage.anonymousFile("ciphertext")
	if err != nil {
		return Manifest{}, err
	}
	cipherSink := &boundedSink{dst: ciphertext, h: sha256.New(), max: options.Limits.MaxCiphertextBytes}
	encryptor, err := age.Encrypt(cipherSink, options.Recipient)
	if err != nil {
		if errors.Is(err, ErrLimit) {
			return Manifest{}, ErrLimit
		}
		return Manifest{}, ErrIO
	}
	plainHash := sha256.New()
	plainBytes, err := copyBounded(ctx, io.MultiWriter(encryptor, plainHash), plaintext, options.Limits.MaxPlaintextBytes)
	if err != nil {
		return Manifest{}, err
	}
	if err := encryptor.Close(); err != nil {
		if errors.Is(err, ErrLimit) {
			return Manifest{}, ErrLimit
		}
		return Manifest{}, ErrIO
	}
	if ciphertext.Sync() != nil {
		return Manifest{}, ErrStaging
	}
	m := Manifest{Format: Format, Metadata: options.Metadata, SigningKeySHA256: digest(key.Public().(ed25519.PublicKey)),
		CiphertextBytes: cipherSink.n, CiphertextSHA256: cipherSink.sum(), PlaintextBytes: plainBytes,
		PlaintextSHA256: hex.EncodeToString(plainHash.Sum(nil))}
	raw, err := json.Marshal(m)
	if err != nil || len(raw) > MaxManifestBytes {
		return Manifest{}, ErrOptions
	}
	signature := ed25519.Sign(key, signatureMessage(raw))
	prefix := append([]byte(Magic), binary.BigEndian.AppendUint32(nil, uint32(len(raw)))...)
	for _, part := range [][]byte{prefix, raw, signature} {
		if err := writeAll(ctx, dst, part); err != nil {
			return Manifest{}, err
		}
	}
	if _, err := ciphertext.Seek(0, io.SeekStart); err != nil {
		return Manifest{}, ErrStaging
	}
	if n, err := copyBounded(ctx, dst, ciphertext, m.CiphertextBytes); err != nil || n != m.CiphertextBytes {
		if err != nil {
			return Manifest{}, err
		}
		return Manifest{}, ErrStaging
	}
	return m, nil
}

// VerifiedArchive has no filesystem path or write API. Its plaintext is an
// owned unlinked private file. Call Close after import/inspection; ignoring a
// Close error is not permitted as a successful cleanup claim. It is not a
// schema-validated/importable archive until the separate importer validates it.
type VerifiedArchive struct {
	mu       sync.Mutex
	manifest Manifest
	file     *os.File
	stage    *staging
	closed   bool
	closeErr error
}

func (v *VerifiedArchive) Manifest() Manifest { return v.manifest }

func (v *VerifiedArchive) Read(p []byte) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed || v.file == nil || v.stage == nil {
		return 0, ErrClosed
	}
	n, err := v.file.Read(p)
	if err != nil && err != io.EOF {
		return n, ErrIO
	}
	return n, err
}

func (v *VerifiedArchive) Seek(offset int64, whence int) (int64, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed || v.file == nil || v.stage == nil {
		return 0, ErrClosed
	}
	n, err := v.file.Seek(offset, whence)
	if err != nil {
		return 0, ErrIO
	}
	return n, nil
}

func (v *VerifiedArchive) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return v.closeErr
	}
	v.closed = true
	if v.stage == nil {
		v.closeErr = ErrClosed
		return v.closeErr
	}
	v.closeErr = v.stage.Close()
	return v.closeErr
}

// Open verifies the externally pinned signer and exact expected snapshot,
// complete ciphertext size/hash and EOF, then fully decrypts into private
// staging. No plaintext reader is returned until age's final authentication,
// plaintext size/hash and staging sync succeed. It never closes caller src.
func Open(ctx context.Context, src io.Reader, options OpenOptions) (_ *VerifiedArchive, resultErr error) {
	if ctx == nil || src == nil || options.Identity == nil || len(options.TrustedSigner) != ed25519.PublicKeySize || !options.Expected.valid() || !options.Limits.valid() {
		return nil, ErrOptions
	}
	if err := canceled(ctx); err != nil {
		return nil, err
	}
	src = &guardedReader{ctx: ctx, src: src}
	// Header reads are bounded; the caller supplies a reader with suitable I/O
	// deadlines. All parser/key errors cross the boundary as fixed safe errors.
	prefix := make([]byte, len(Magic)+4)
	if err := readHeader(ctx, src, prefix); err != nil {
		return nil, err
	}
	if string(prefix[:len(Magic)]) != Magic {
		return nil, ErrInvalid
	}
	size := binary.BigEndian.Uint32(prefix[len(Magic):])
	if size == 0 || size > MaxManifestBytes {
		return nil, ErrInvalid
	}
	raw, signature := make([]byte, size), make([]byte, ed25519.SignatureSize)
	if err := readHeader(ctx, src, raw); err != nil {
		return nil, err
	}
	if err := readHeader(ctx, src, signature); err != nil {
		return nil, err
	}
	m, err := readManifest(raw, options.Limits)
	if err != nil {
		return nil, err
	}
	if m.Metadata != options.Expected || m.SigningKeySHA256 != digest(options.TrustedSigner) || !ed25519.Verify(options.TrustedSigner, signatureMessage(raw), signature) {
		return nil, ErrInvalid
	}
	if err := canceled(ctx); err != nil {
		return nil, err
	}
	stage, err := newStaging(options.StagingParent)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep && stage.Close() != nil {
			resultErr = ErrStaging
		}
	}()
	ciphertext, err := stage.anonymousFile("ciphertext")
	if err != nil {
		return nil, err
	}
	sink := &boundedSink{dst: ciphertext, h: sha256.New(), max: m.CiphertextBytes}
	n, err := copyBounded(ctx, sink, src, m.CiphertextBytes)
	if err != nil {
		if err == ErrCanceled {
			return nil, err
		}
		return nil, ErrInvalid
	}
	if n != m.CiphertextBytes || sink.sum() != m.CiphertextSHA256 {
		return nil, ErrInvalid
	}
	if _, err := ciphertext.Seek(0, io.SeekStart); err != nil {
		return nil, ErrStaging
	}
	decryptor, err := age.Decrypt(ciphertext, options.Identity)
	if err != nil {
		return nil, ErrInvalid
	}
	plaintext, err := stage.anonymousFile("plaintext")
	if err != nil {
		return nil, err
	}
	plainSink := &boundedSink{dst: plaintext, h: sha256.New(), max: m.PlaintextBytes}
	// Reading until real EOF is required even for an exact-size last chunk.
	// LimitReader alone would hide age's delayed final/trailing-data error.
	n, err = copyBounded(ctx, plainSink, decryptor, m.PlaintextBytes)
	if err != nil {
		if err == ErrCanceled {
			return nil, err
		}
		return nil, ErrInvalid
	}
	if n != m.PlaintextBytes || plainSink.sum() != m.PlaintextSHA256 {
		return nil, ErrInvalid
	}
	if plaintext.Sync() != nil {
		return nil, ErrStaging
	}
	if _, err := plaintext.Seek(0, io.SeekStart); err != nil {
		return nil, ErrStaging
	}
	keep = true
	return &VerifiedArchive{manifest: m, file: plaintext, stage: stage}, nil
}
