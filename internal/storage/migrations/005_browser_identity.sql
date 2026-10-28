-- SPDX-License-Identifier: Elastic-2.0
CREATE TABLE identity_mappings (
 issuer text NOT NULL,subject text NOT NULL,principal_id text NOT NULL REFERENCES principals(id),active boolean NOT NULL DEFAULT true,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),PRIMARY KEY(issuer,subject)
);
CREATE INDEX identity_mapping_principal ON identity_mappings(principal_id);
CREATE TABLE login_transactions (
 state_hash text PRIMARY KEY,binding_hash text NOT NULL,issuer text NOT NULL,client_id text NOT NULL,redirect_uri text NOT NULL,
 nonce text NOT NULL,code_verifier_ciphertext bytea NOT NULL,
 created_at timestamptz NOT NULL,expires_at timestamptz NOT NULL,consumed_at timestamptz,
 CHECK(expires_at>created_at),CHECK(expires_at<=created_at+interval '10 minutes')
);
CREATE INDEX login_expiry ON login_transactions(expires_at);
CREATE TABLE browser_sessions (
 id text NOT NULL UNIQUE,token_hash text PRIMARY KEY,principal_id text NOT NULL REFERENCES principals(id),issuer text NOT NULL,subject text NOT NULL,
 authenticated_at timestamptz NOT NULL,created_at timestamptz NOT NULL,expires_at timestamptz NOT NULL,revoked boolean NOT NULL DEFAULT false,
 FOREIGN KEY(issuer,subject) REFERENCES identity_mappings(issuer,subject),
 CHECK(expires_at>created_at),CHECK(authenticated_at<=created_at),CHECK(expires_at<=authenticated_at+interval '24 hours')
);
CREATE INDEX session_principal ON browser_sessions(principal_id,expires_at);
