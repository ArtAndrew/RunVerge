// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentplatform.local/platform/internal/recoveryarchive"
	"filippo.io/age"
)

type fixture struct {
	identity *age.X25519Identity
	public   ed25519.PublicKey
	private  ed25519.PrivateKey
	metadata recoveryarchive.Metadata
	limits   recoveryarchive.Limits
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{identity, public, private, recoveryarchive.Metadata{
		BackupID: "backup_0123456789abcdef0123456789abcdef", SourceID: "source_fedcba9876543210fedcba9876543210",
		CutUTC: "2026-10-28T12:34:56.123456789Z", SchemaSHA256: strings.Repeat("a", 64),
	}, recoveryarchive.Limits{MaxPlaintextBytes: 1 << 20, MaxCiphertextBytes: 2 << 20}}
}

func privateDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

func (f fixture) sealOptions(stage string) recoveryarchive.SealOptions {
	return recoveryarchive.SealOptions{Recipient: f.identity.Recipient(), SigningKey: f.private, Metadata: f.metadata, Limits: f.limits, StagingParent: stage}
}

func (f fixture) openOptions(stage string) recoveryarchive.OpenOptions {
	return recoveryarchive.OpenOptions{Identity: f.identity, TrustedSigner: f.public, Expected: f.metadata, Limits: f.limits, StagingParent: stage}
}

// independentEnvelope uses the upstream age writer and standard Ed25519
// directly. It does not call Seal, so Open is also tested on separately built
// bytes, including correctly signed corrupt ciphertext and false size claims.
func (f fixture) independentEnvelope(t *testing.T, plaintext []byte, transformCipher func([]byte) []byte, transformManifest func(*recoveryarchive.Manifest)) []byte {
	t.Helper()
	var encrypted bytes.Buffer
	w, err := age.Encrypt(&encrypted, f.identity.Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write(plaintext); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	cipher := encrypted.Bytes()
	if transformCipher != nil {
		cipher = transformCipher(cipher)
	}
	m := recoveryarchive.Manifest{Format: "runverge-recovery-age-x25519-ed25519-v1", Metadata: f.metadata,
		SigningKeySHA256: digest(f.public), CiphertextBytes: int64(len(cipher)), CiphertextSHA256: digest(cipher),
		PlaintextBytes: int64(len(plaintext)), PlaintextSHA256: digest(plaintext)}
	if transformManifest != nil {
		transformManifest(&m)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return f.frame(raw, cipher)
}

func (f fixture) frame(raw, ciphertext []byte) []byte {
	message := append([]byte("RunVerge recovery archive signature v1\x00"), binary.BigEndian.AppendUint32(nil, uint32(len(raw)))...)
	message = append(message, raw...)
	result := append([]byte("RUNVERGE-RECOVERY-1\n"), binary.BigEndian.AppendUint32(nil, uint32(len(raw)))...)
	result = append(result, raw...)
	result = append(result, ed25519.Sign(f.private, message)...)
	return append(result, ciphertext...)
}

func assertEmpty(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging cleanup: count=%d err=%v", len(entries), err)
	}
}

func rejected(t *testing.T, raw []byte, options recoveryarchive.OpenOptions) error {
	t.Helper()
	archive, err := recoveryarchive.Open(context.Background(), bytes.NewReader(raw), options)
	if archive != nil || err == nil {
		if archive != nil {
			_ = archive.Close()
		}
		t.Fatal("invalid envelope exposed a plaintext reader")
	}
	assertEmpty(t, options.StagingParent)
	if strings.Contains(err.Error(), options.Identity.String()) || strings.Contains(err.Error(), options.StagingParent) {
		t.Fatal("error exposed private input")
	}
	return err
}

func TestAuthorRoundTripRealAgeChunkBoundaries(t *testing.T) {
	f := newFixture(t)
	for _, size := range []int{0, 1, 65535, 65536, 65537, 131073} {
		t.Run(stringName(size), func(t *testing.T) {
			plaintext := bytes.Repeat([]byte{byte(size % 251)}, size)
			stage := privateDirectory(t)
			var output bytes.Buffer
			manifest, err := recoveryarchive.Seal(context.Background(), &output, bytes.NewReader(plaintext), f.sealOptions(stage))
			if err != nil {
				t.Fatal(err)
			}
			assertEmpty(t, stage)
			archive, err := recoveryarchive.Open(context.Background(), bytes.NewReader(output.Bytes()), f.openOptions(stage))
			if err != nil {
				t.Fatal(err)
			}
			if archive.Manifest() != manifest {
				t.Fatal("manifest changed")
			}
			observed, err := io.ReadAll(archive)
			if err != nil || !bytes.Equal(observed, plaintext) {
				t.Fatal("plaintext mismatch")
			}
			if _, err = archive.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			if again, err := io.ReadAll(archive); err != nil || !bytes.Equal(again, plaintext) {
				t.Fatal("seek lost exact content")
			}
			if err = archive.Close(); err != nil {
				t.Fatal(err)
			}
			if err = archive.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err = archive.Read(make([]byte, 1)); err != recoveryarchive.ErrClosed {
				t.Fatal("closed archive still readable")
			}
			assertEmpty(t, stage)
		})
	}
}

func stringName(size int) string {
	if size == 0 {
		return "empty"
	}
	var digits [20]byte
	position := len(digits)
	for size > 0 {
		position--
		digits[position] = byte('0' + size%10)
		size /= 10
	}
	return string(digits[position:])
}

func TestAuthorIndependentAgeEnvelopeAndNoNamedPlaintext(t *testing.T) {
	f, stage := newFixture(t), privateDirectory(t)
	plain := bytes.Repeat([]byte("representative historical artifact\x00"), 4096)
	raw := f.independentEnvelope(t, plain, nil, nil)
	archive, err := recoveryarchive.Open(context.Background(), bytes.NewReader(raw), f.openOptions(stage))
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if err = filepath.Walk(stage, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			t.Fatal("named ciphertext/plaintext remains in staging")
		}
		if info.Mode().Perm() != 0700 {
			t.Fatal("staging directory is not private")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(archive)
	if err != nil || !bytes.Equal(actual, plain) {
		t.Fatal("independent encrypted fixture mismatch")
	}
}

func TestAuthorSealUsesStandardAgeAndBoundEd25519(t *testing.T) {
	f, stage := newFixture(t), privateDirectory(t)
	var output bytes.Buffer
	plain := []byte("data-only independent decrypt check")
	if _, err := recoveryarchive.Seal(context.Background(), &output, bytes.NewReader(plain), f.sealOptions(stage)); err != nil {
		t.Fatal(err)
	}
	raw := output.Bytes()
	offset := len("RUNVERGE-RECOVERY-1\n")
	size := int(binary.BigEndian.Uint32(raw[offset : offset+4]))
	offset += 4
	header, signature := raw[offset:offset+size], raw[offset+size:offset+size+64]
	message := append([]byte("RunVerge recovery archive signature v1\x00"), binary.BigEndian.AppendUint32(nil, uint32(size))...)
	message = append(message, header...)
	if !ed25519.Verify(f.public, message, signature) {
		t.Fatal("independent signature verification failed")
	}
	reader, err := age.Decrypt(bytes.NewReader(raw[offset+size+64:]), f.identity)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(decoded, plain) {
		t.Fatal("independent upstream decryption failed")
	}
}

func TestAuthorWrongKeysAndAuthenticWrongSnapshotAreRejected(t *testing.T) {
	f, other, stage := newFixture(t), newFixture(t), privateDirectory(t)
	raw := f.independentEnvelope(t, []byte("historical data"), nil, nil)
	for _, field := range []string{"signer", "identity", "backup", "source", "cut", "schema"} {
		t.Run(field, func(t *testing.T) {
			options := f.openOptions(stage)
			switch field {
			case "signer":
				options.TrustedSigner = other.public
			case "identity":
				options.Identity = other.identity
			case "backup":
				options.Expected.BackupID = "backup_" + strings.Repeat("b", 32)
			case "source":
				options.Expected.SourceID = "source_" + strings.Repeat("c", 32)
			case "cut":
				options.Expected.CutUTC = "2026-10-28T12:34:57Z"
			case "schema":
				options.Expected.SchemaSHA256 = strings.Repeat("d", 64)
			}
			if err := rejected(t, raw, options); err != recoveryarchive.ErrInvalid {
				t.Fatal(err)
			}
		})
	}
}

func TestAuthorTamperTruncationAndUnauthenticatedTrailingBytes(t *testing.T) {
	f, stage := newFixture(t), privateDirectory(t)
	raw := f.independentEnvelope(t, bytes.Repeat([]byte("x"), 131072), nil, nil)
	for _, offset := range []int{0, 10, len(recoveryarchive.Magic), 100, len(raw) - 1} {
		t.Run("tamper-"+stringName(offset), func(t *testing.T) {
			changed := append([]byte(nil), raw...)
			changed[offset] ^= 1
			rejected(t, changed, f.openOptions(stage))
		})
	}
	for _, cut := range []int{0, 1, len(recoveryarchive.Magic) + 2, 64, len(raw) - 1, len(raw) - 16, len(raw) - 65552} {
		t.Run("truncate-"+stringName(cut), func(t *testing.T) { rejected(t, raw[:cut], f.openOptions(stage)) })
	}
	for _, suffix := range [][]byte{{0}, []byte("untrusted trailing content"), raw} {
		t.Run("trailing-"+stringName(len(suffix)), func(t *testing.T) {
			rejected(t, append(append([]byte(nil), raw...), suffix...), f.openOptions(stage))
		})
	}
}

func TestAuthorSignedCorruptAgeAndFalsePlaintextClaimsNeverExposeReader(t *testing.T) {
	f, stage := newFixture(t), privateDirectory(t)
	plain := bytes.Repeat([]byte("z"), 131072)
	cases := []struct {
		name     string
		cipher   func([]byte) []byte
		manifest func(*recoveryarchive.Manifest)
	}{
		{"age-final-tag", func(raw []byte) []byte { raw[len(raw)-1] ^= 1; return raw }, nil},
		{"age-truncated", func(raw []byte) []byte { return raw[:len(raw)-16] }, nil},
		{"age-full-final-with-trailing", func(raw []byte) []byte { return append(raw, 42) }, nil},
		{"plain-hash", nil, func(m *recoveryarchive.Manifest) { m.PlaintextSHA256 = strings.Repeat("0", 64) }},
		{"plain-short", nil, func(m *recoveryarchive.Manifest) { m.PlaintextBytes-- }},
		{"plain-long", nil, func(m *recoveryarchive.Manifest) { m.PlaintextBytes++ }},
		{"cipher-hash", nil, func(m *recoveryarchive.Manifest) { m.CiphertextSHA256 = strings.Repeat("0", 64) }},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			raw := f.independentEnvelope(t, plain, item.cipher, item.manifest)
			if err := rejected(t, raw, f.openOptions(stage)); err != recoveryarchive.ErrInvalid {
				t.Fatal(err)
			}
		})
	}
}

func TestAuthorStrictCanonicalHeaderRejectsSignedAmbiguities(t *testing.T) {
	f, stage := newFixture(t), privateDirectory(t)
	original := f.independentEnvelope(t, []byte("row"), nil, nil)
	start := len(recoveryarchive.Magic) + 4
	size := int(binary.BigEndian.Uint32(original[start-4 : start]))
	raw, cipher := original[start:start+size], original[start+size+64:]
	cases := map[string][]byte{
		"duplicate":        bytes.Replace(raw, []byte(`"format":`), []byte(`"format":"ignored","format":`), 1),
		"unknown":          append(append([]byte(nil), raw[:len(raw)-1]...), []byte(`,"extra":"untrusted"}`)...),
		"case-folded":      bytes.Replace(raw, []byte(`"metadata"`), []byte(`"Metadata"`), 1),
		"whitespace":       append(append([]byte(nil), raw...), '\n'),
		"null-metadata":    bytes.Replace(raw, raw[bytes.Index(raw, []byte(`{"backup_id"`)):bytes.Index(raw, []byte(`},"signing_key_sha256"`))+1], []byte("null"), 1),
		"negative-length":  bytes.Replace(raw, []byte(`"plaintext_bytes":3`), []byte(`"plaintext_bytes":-3`), 1),
		"overflow-length":  bytes.Replace(raw, []byte(`"plaintext_bytes":3`), []byte(`"plaintext_bytes":9223372036854775808`), 1),
		"noncanonical-utc": bytes.Replace(raw, []byte(`2026-10-28T12:34:56.123456789Z`), []byte(`2026-10-28T12:34:56.123456789+00:00`), 1),
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			rejected(t, f.frame(header, cipher), f.openOptions(stage))
		})
	}
}

func TestAuthorLimitsAreCheckedBeforeExposeOrOutput(t *testing.T) {
	f, stage := newFixture(t), privateDirectory(t)
	plaintext := bytes.Repeat([]byte("p"), 1000)
	raw := f.independentEnvelope(t, plaintext, nil, nil)
	for _, which := range []string{"plain", "cipher"} {
		t.Run(which, func(t *testing.T) {
			options := f.openOptions(stage)
			if which == "plain" {
				options.Limits.MaxPlaintextBytes = 999
			} else {
				options.Limits.MaxCiphertextBytes = 100
			}
			if err := rejected(t, raw, options); err != recoveryarchive.ErrLimit {
				t.Fatal(err)
			}
			seal := f.sealOptions(stage)
			seal.Limits = options.Limits
			var out bytes.Buffer
			if _, err := recoveryarchive.Seal(context.Background(), &out, bytes.NewReader(plaintext), seal); err != recoveryarchive.ErrLimit {
				t.Fatal(err)
			}
			if out.Len() != 0 {
				t.Fatal("limit failure emitted a partial output before input completed")
			}
			assertEmpty(t, stage)
		})
	}
	options := f.openOptions(stage)
	options.Limits.MaxPlaintextBytes = recoveryarchive.MaxStagedBytes
	if err := rejected(t, raw, options); err != recoveryarchive.ErrOptions {
		t.Fatal(err)
	}
}

type failReader struct {
	data  []byte
	calls int
}

func (r *failReader) Read(p []byte) (int, error) {
	r.calls++
	if len(r.data) == 0 {
		return 0, errors.New("private-sentinel-secret-source-path")
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

type noProgressReader struct{}

func (noProgressReader) Read([]byte) (int, error) { return 0, nil }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

type cancelReader struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r cancelReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.cancel()
	return n, err
}

func TestAuthorReadErrorsNoProgressAndShortWriteAreSafe(t *testing.T) {
	f, stage := newFixture(t), privateDirectory(t)
	for _, input := range []io.Reader{&failReader{data: []byte("partial sensitive input")}, noProgressReader{}} {
		var output bytes.Buffer
		if _, err := recoveryarchive.Seal(context.Background(), &output, input, f.sealOptions(stage)); err != recoveryarchive.ErrIO {
			t.Fatal(err)
		}
		if output.Len() != 0 {
			t.Fatal("failed source was published")
		}
		assertEmpty(t, stage)
	}
	if _, err := recoveryarchive.Seal(context.Background(), shortWriter{}, strings.NewReader("data"), f.sealOptions(stage)); err != recoveryarchive.ErrIO {
		t.Fatal(err)
	}
	assertEmpty(t, stage)
	raw := f.independentEnvelope(t, []byte("row"), nil, nil)
	for _, input := range []io.Reader{&failReader{data: raw}, noProgressReader{}} {
		archive, err := recoveryarchive.Open(context.Background(), input, f.openOptions(stage))
		if archive != nil || err != recoveryarchive.ErrInvalid {
			t.Fatal("read error exposed plaintext")
		}
		assertEmpty(t, stage)
	}
}

func TestAuthorCancellationBeforeAndDuringIO(t *testing.T) {
	f, stage := newFixture(t), privateDirectory(t)
	raw := f.independentEnvelope(t, []byte("row"), nil, nil)
	for _, early := range []bool{true, false} {
		t.Run(map[bool]string{true: "before", false: "during"}[early], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var reader io.Reader = bytes.NewReader(raw)
			if early {
				cancel()
			} else {
				reader = cancelReader{reader, cancel}
			}
			if archive, err := recoveryarchive.Open(ctx, reader, f.openOptions(stage)); archive != nil || err != recoveryarchive.ErrCanceled {
				t.Fatalf("open cancel: %v", err)
			}
			ctx, cancel = context.WithCancel(context.Background())
			defer cancel()
			reader = strings.NewReader("data")
			if early {
				cancel()
			} else {
				reader = cancelReader{reader, cancel}
			}
			var out bytes.Buffer
			if _, err := recoveryarchive.Seal(ctx, &out, reader, f.sealOptions(stage)); err != recoveryarchive.ErrCanceled {
				t.Fatal(err)
			}
			if out.Len() != 0 {
				t.Fatal("canceled source was published")
			}
			assertEmpty(t, stage)
		})
	}
}

func TestAuthorInvalidKeysMetadataAndPrivateStaging(t *testing.T) {
	f, stage := newFixture(t), privateDirectory(t)
	for _, key := range []ed25519.PrivateKey{nil, make([]byte, 32), make([]byte, 64)} {
		options := f.sealOptions(stage)
		options.SigningKey = key
		var out bytes.Buffer
		if _, err := recoveryarchive.Seal(context.Background(), &out, strings.NewReader("data"), options); err != recoveryarchive.ErrOptions {
			t.Fatal(err)
		}
	}
	for _, cut := range []string{"", "2026-10-28T12:34:56+00:00", "2026-10-28T12:34:56.000Z", "not-a-date"} {
		options := f.sealOptions(stage)
		options.Metadata.CutUTC = cut
		if _, err := recoveryarchive.Seal(context.Background(), io.Discard, strings.NewReader("data"), options); err != recoveryarchive.ErrOptions {
			t.Fatal(err)
		}
	}
	for _, mode := range []os.FileMode{0755, 0777} {
		if err := os.Chmod(stage, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := recoveryarchive.Seal(context.Background(), io.Discard, strings.NewReader("data"), f.sealOptions(stage)); err != recoveryarchive.ErrStaging {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(stage, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(privateDirectory(t), "linked-stage")
	if err := os.Symlink(stage, link); err != nil {
		t.Fatal(err)
	}
	if _, err := recoveryarchive.Seal(context.Background(), io.Discard, strings.NewReader("data"), f.sealOptions(link)); err != recoveryarchive.ErrStaging {
		t.Fatal(err)
	}
	assertEmpty(t, stage)
}
