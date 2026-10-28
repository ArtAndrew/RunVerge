# Recovery archive envelope

SPDX-License-Identifier: Elastic-2.0

This internal module provides one necessary part of recovery: an authenticated,
encrypted, bounded opaque stream. It does not export/import PostgreSQL data,
validate table contents, fence the source, grant authority or release execution
holds. It is not complete backup/restore or alpha acceptance.

## Reviewed dependency and scope

Encryption uses pinned `filippo.io/age v1.3.2`, the stable release dated
29 August 2026. The supported profile uses one native X25519 recipient and the
corresponding X25519 identity; SSH, passwords, interactive plugins, generic
caller-supplied key implementations and hybrid recipients are not accepted by
this API. Hybrid support would require a separately reviewed profile.

The official [release notes](https://github.com/FiloSottile/age/releases/tag/v1.3.2),
[versioned Go API](https://pkg.go.dev/filippo.io/age@v1.3.2),
[versioned module dependencies](https://github.com/FiloSottile/age/blob/v1.3.2/go.mod)
and [age format](https://age-encryption.org/v1) were checked on 28 October 2026.
`age.Encrypt` requires closing its writer to emit the final encrypted chunk;
`age.Decrypt` returns a streaming reader whose errors must also be checked while
reading. The module consumes that reader through EOF before handing out any
plaintext. It does not implement its own chunk cipher or nonce scheme.

## API and trust

`Seal(ctx, dst, plaintext, SealOptions) (Manifest, error)` accepts a recipient
public key, a separate Ed25519 signing private key, exact snapshot metadata,
size limits and a private staging parent. The source does not need the target's
decryption identity. A failed input produces no output; output failure can leave
a partial envelope, which must not be published as a successful backup.

`Open(ctx, src, OpenOptions) (*VerifiedArchive, error)` requires the decryption
identity, an independently trusted Ed25519 public key, and **all** expected
snapshot metadata. It never learns trust from a signing key inside the archive.
The signing-key fingerprint is only an exact binding to the supplied key.
Expected backup ID, source ID, cutoff and schema hash prevent an otherwise valid
signature on a stale or different snapshot from satisfying this selection.

`VerifiedArchive` implements `Read`, `Seek`, and `Close`; `Manifest()` returns a
value copy. No path, write method or underlying descriptor is exposed. This is
an authenticated opaque byte stream, not evidence that the data is safe to
import. The separate importer must still validate all entries, columns, counts,
types, references, sequence semantics and accounting invariants before mutation.

The caller owns input/output streams and must check their `Close`/`Sync` errors,
durably publish the output, and bind the selected metadata to trusted operator
evidence. The module cannot prove that a timestamp represents a real consistent
database snapshot or that a source is fenced. Signing-key rotation/revocation,
current ownership and accepted backup selection remain caller policy; no
automatic fallback to archived, expired or revoked keys occurs here.

## Exact envelope v1

The byte sequence is:

1. ASCII magic `RUNVERGE-RECOVERY-1\n`.
2. A four-byte big-endian unsigned length, from 1 through 4096.
3. Exactly that many canonical JSON manifest bytes.
4. Exactly 64 bytes of Ed25519 signature.
5. Exactly `ciphertext_bytes` bytes containing one standard binary age file.
6. End of input; any appended content or another envelope is rejected.

The Ed25519 message is `RunVerge recovery archive signature v1\x00`, followed by
the same four-byte manifest length and exact manifest bytes. Go `json.Marshal`
of the fixed `Manifest` struct defines canonical representation and field order.
The decoder re-encodes and requires byte equality: duplicate or unknown fields,
case aliases, null/missing values, alternate numbers, extra JSON and whitespace
are rejected. This is a versioned internal format, not general JSON
canonicalization or an open interoperability claim.

Signed fields include format, backup/source IDs, normalized UTC cutoff, schema
SHA-256, trusted signing-key fingerprint, ciphertext length/SHA-256 and plaintext
length/SHA-256. IDs follow `domain.NewID` shape: a lowercase alphanumeric prefix
starting with a letter, underscore, then 32 lowercase hexadecimal characters.
Cutoffs are exact UTC `time.RFC3339Nano` strings ending in `Z`, years 2000–9999;
hashes are 64 lowercase hex characters. Metadata and hashes remain visible in
the envelope; store the backup as private operational data. Do not put secrets
in IDs or publish this envelope as a release artifact.

## Bounded staging and failures

Every call sets positive plaintext and ciphertext limits. Their combined sum
must not exceed the hard 1 TiB ceiling; deployments should set substantially
smaller limits for their actual data. Allocation is streaming, not proportional
to those configured limits. Manifest allocation is capped at 4096 bytes; payload
copy buffers are 64 KiB plus age's bounded internal parsing/stream buffers.
No compression or extraction is performed here.

The first staging profile is POSIX, implemented for macOS/Linux and tested on
macOS arm64. Other platforms refuse the operation. The caller supplies an
absolute normalized existing directory owned by its OS user with mode0700.
Its ancestors and the OS account are trusted. The module creates a random owned
subdirectory and exclusively creates mode0600 files. Files are immediately
unlinked, remaining accessible only through owned descriptors; they are not
paths an importer can reopen or replace. Ordinary success/error cleanup closes
descriptors and removes only the empty owned directory. Unexpected contents are
never recursively deleted. Cleanup failures remain errors, including on repeated
`Close`; they must not be reported as successful cleanup. A process crash can
leave a private staging directory; operating-system and disk encryption are
separate responsibilities. No secure physical erasure is claimed.

`Open` authenticates the expected metadata/signature, stages and validates the
complete ciphertext including EOF, then decrypts to a separate anonymous private
file. The whole plaintext hash, size and age final authentication must succeed
before `VerifiedArchive` is returned. Signed malformed ciphertext still fails.

Errors are fixed sentinels. Underlying parser, key, filesystem and transport
errors are not echoed or wrapped with secrets/paths. Context cancellation is
checked between I/O calls, and repeated zero-progress reads are refused. A
blocked arbitrary reader/writer cannot be safely interrupted by this module;
the caller must set transport deadlines and own cancellation/closure. This
limitation is not a claim that an elapsed timeout stopped another process.

## Verification

Author tests use real age and Ed25519. Separate fixture assembly exercises Open
without using Seal, and another test checks Seal using upstream decryption and
standard Ed25519 verification. Cases include exact64KiB boundaries, empty input,
wrong keys and source selection, truncation, appended data, signed inner-AEAD
failure, false plaintext claims, JSON ambiguity, bounds, reader/output failures,
cancellation and private cleanup. These are author tests, not independent
security acceptance or a database recovery drill.
