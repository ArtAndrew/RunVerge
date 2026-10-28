// SPDX-License-Identifier: Elastic-2.0
package storage

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/recoveryarchive"
	"filippo.io/age"
)

// This connects the real populated PG exporter to the authenticated envelope.
// It deliberately performs no import and grants no restored authority.
func TestRecoverySnapshotEnvelopeIntegration(t *testing.T) {
	s, _ := recoveryAuthorStore(t)
	recoveryAuthorPopulate(t, s)
	ctx := context.Background()
	var plain bytes.Buffer
	snapshot, err := s.ExportRecoverySnapshot(ctx, &plain, DefaultRecoverySnapshotLimits())
	if err != nil {
		t.Fatal("populated snapshot failed", err)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal("recipient generation failed")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal("signer generation failed")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal("private staging setup failed")
	}
	selected := recoveryarchive.Metadata{
		BackupID: snapshot.Metadata.BackupID, SourceID: snapshot.Metadata.SourceID,
		CutUTC: snapshot.Metadata.CutUTC, SchemaSHA256: snapshot.Metadata.SchemaSHA256,
	}
	limits := recoveryarchive.Limits{MaxPlaintextBytes: 8 << 20, MaxCiphertextBytes: 9 << 20}
	sealOptions := recoveryarchive.SealOptions{Recipient: identity.Recipient(), SigningKey: private, Metadata: selected, Limits: limits, StagingParent: parent}
	openOptions := recoveryarchive.OpenOptions{Identity: identity, TrustedSigner: public, Expected: selected, Limits: limits, StagingParent: parent}
	var sealed bytes.Buffer
	manifest, err := recoveryarchive.Seal(ctx, &sealed, bytes.NewReader(plain.Bytes()), sealOptions)
	if err != nil || manifest.PlaintextBytes != int64(plain.Len()) || manifest.PlaintextSHA256 != Hash(plain.Bytes()) {
		t.Fatal("snapshot envelope binding failed", err)
	}
	t.Run("exact_authenticated_snapshot", func(t *testing.T) {
		verified, err := recoveryarchive.Open(ctx, bytes.NewReader(sealed.Bytes()), openOptions)
		if err != nil {
			t.Fatal("authenticated snapshot open failed", err)
		}
		t.Cleanup(func() {
			if err := verified.Close(); err != nil {
				t.Error("verified snapshot cleanup failed")
			}
		})
		validated, err := ValidateRecoverySnapshot(ctx, verified, DefaultRecoverySnapshotLimits())
		if err != nil || !reflect.DeepEqual(snapshot, validated) || verified.Manifest() != manifest {
			t.Fatal("inner snapshot and selected envelope differ", err)
		}
		if _, err := verified.Seek(0, io.SeekStart); err != nil {
			t.Fatal("authenticated rewind failed")
		}
		raw, err := io.ReadAll(io.LimitReader(verified, int64(plain.Len())+1))
		if err != nil || !bytes.Equal(raw, plain.Bytes()) {
			t.Fatal("artifact, history or liability bytes changed during encryption")
		}
	})
	t.Run("different_selected_backup", func(t *testing.T) {
		options := openOptions
		options.Expected.BackupID = domain.NewID("bak")
		verified, err := recoveryarchive.Open(ctx, bytes.NewReader(sealed.Bytes()), options)
		if verified != nil || !errors.Is(err, recoveryarchive.ErrInvalid) {
			t.Fatal("signed different backup exposed plaintext")
		}
	})
	t.Run("signed_invalid_inner_snapshot", func(t *testing.T) {
		// A trusted signature authenticates bytes, not PostgreSQL semantics.
		var malformed bytes.Buffer
		if _, err := recoveryarchive.Seal(ctx, &malformed, bytes.NewReader(append(append([]byte{}, plain.Bytes()...), byte(' '))), sealOptions); err != nil {
			t.Fatal("signed malformed fixture failed")
		}
		verified, err := recoveryarchive.Open(ctx, &malformed, openOptions)
		if err != nil {
			t.Fatal("opaque authentication unexpectedly interpreted snapshot")
		}
		t.Cleanup(func() {
			if err := verified.Close(); err != nil {
				t.Error("malformed fixture cleanup failed")
			}
		})
		if _, err := ValidateRecoverySnapshot(ctx, verified, DefaultRecoverySnapshotLimits()); !errors.Is(err, ErrRecoverySnapshotInvalid) {
			t.Fatal("signature bypassed inner snapshot validation")
		}
	})
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatal("envelope staging remained after verification")
	}
	var again bytes.Buffer
	after, err := s.ExportRecoverySnapshot(ctx, &again, DefaultRecoverySnapshotLimits())
	if err != nil || !reflect.DeepEqual(snapshot.Tables, after.Tables) || !reflect.DeepEqual(snapshot.Sequences, after.Sequences) {
		t.Fatal("archive integration changed source history or liabilities", err)
	}
}
