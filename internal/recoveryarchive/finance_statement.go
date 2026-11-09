// SPDX-License-Identifier: Elastic-2.0
package recoveryarchive

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

const financeStatementDomain = "RunVerge final finance statement v1\x00"

var ErrFinanceStatement = errors.New("final finance statement or its scoped signer could not be verified")
var financeID = regexp.MustCompile("^[a-f0-9]{32}$")
var financeReference = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$")

// The host pins a finance reviewer for one account and organization on one
// activation. Its signature attests that the reviewer checked the final bill.
type FinanceScope struct {
	TargetID       string `json:"target_id"`
	RestoreID      string `json:"restore_id"`
	ActivationID   string `json:"activation_id"`
	OrganizationID string `json:"organization_id"`
	Account        string `json:"provider_account"`
}

type FinanceStatement struct {
	Version          int          `json:"version"`
	Scope            FinanceScope `json:"scope"`
	ObservationID    string       `json:"observation_id"`
	AcquisitionSHA   string       `json:"acquisition_sha256"`
	InvoiceLine      string       `json:"invoice_line"`
	DocumentSHA      string       `json:"document_sha256"`
	AmountMicroUSD   int64        `json:"amount_microusd,string"`
	ExpectedRevision int          `json:"expected_revision"`
	CorrectionReason string       `json:"correction_reason,omitempty"`
}

type financeTrust struct {
	Version int          `json:"version"`
	Scope   FinanceScope `json:"scope"`
	Key     []byte       `json:"public_key"`
}
type signedFinanceStatement struct {
	Statement FinanceStatement `json:"statement"`
	Signature []byte           `json:"signature"`
}
type VerifiedFinanceStatement struct {
	statement FinanceStatement
	sha256    string
	signer    string
	envelope  []byte
	publicKey []byte
}

func (v VerifiedFinanceStatement) Statement() FinanceStatement { return v.statement }
func (v VerifiedFinanceStatement) SHA256() string              { return v.sha256 }
func (v VerifiedFinanceStatement) SignerSHA256() string        { return v.signer }
func (v VerifiedFinanceStatement) RetainedProof() ([]byte, []byte) {
	return bytes.Clone(v.envelope), bytes.Clone(v.publicKey)
}

func financeCanonical(raw []byte, value any) bool {
	if json.Unmarshal(raw, value) != nil {
		return false
	}
	body, err := json.Marshal(value)
	var compact bytes.Buffer
	return err == nil && json.Compact(&compact, raw) == nil && bytes.Equal(body, compact.Bytes())
}

func VerifyHostFinanceStatement(path, trustPath, documentPath string) (VerifiedFinanceStatement, error) {
	var empty VerifiedFinanceStatement
	trustBytes, err := readHostPrivateFile(trustPath, 4096)
	var trust financeTrust
	if err != nil || !financeCanonical(trustBytes, &trust) || trust.Version != 1 || len(trust.Key) != ed25519.PublicKeySize {
		return empty, ErrFinanceStatement
	}
	raw, err := readHostPrivateFile(path, 16384)
	st, canonical, ok := verifyFinanceEnvelope(raw, trust.Key)
	if err != nil || !ok || st.Scope != trust.Scope {
		return empty, ErrFinanceStatement
	}
	document, err := ReadHostPrivateFile(documentPath)
	defer clear(document)
	if err != nil || len(document) == 0 || digest(document) != st.DocumentSHA {
		return empty, ErrFinanceStatement
	}
	return VerifiedFinanceStatement{st, digest(canonical), digest(trust.Key), canonical, trust.Key}, nil
}

// RetainedFinanceStatement verifies historical representation without
// granting current financial authority or constructing an opaque proof.
func RetainedFinanceStatement(raw, key []byte) (FinanceStatement, bool) {
	st, canonical, ok := verifyFinanceEnvelope(raw, key)
	return st, ok && bytes.Equal(raw, canonical)
}

func verifyFinanceEnvelope(raw, key []byte) (FinanceStatement, []byte, bool) {
	var signed signedFinanceStatement
	if len(raw) > 16384 || len(key) != ed25519.PublicKeySize || !financeCanonical(raw, &signed) {
		return FinanceStatement{}, nil, false
	}
	st := signed.Statement
	if st.Version != 1 || !financeID.MatchString(st.ObservationID) || !digestPattern.MatchString(st.AcquisitionSHA) ||
		!financeReference.MatchString(st.InvoiceLine) || !digestPattern.MatchString(st.DocumentSHA) || st.AmountMicroUSD < 0 ||
		st.ExpectedRevision < 1 || len(st.CorrectionReason) > 256 ||
		(st.CorrectionReason != "" && strings.TrimSpace(st.CorrectionReason) == "") {
		return FinanceStatement{}, nil, false
	}
	body, _ := json.Marshal(st)
	if !ed25519.Verify(key, append([]byte(financeStatementDomain), body...), signed.Signature) {
		return FinanceStatement{}, nil, false
	}
	canonical, _ := json.Marshal(signed)
	return st, canonical, true
}
