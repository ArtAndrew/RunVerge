-- SPDX-License-Identifier: Elastic-2.0
ALTER TABLE actions ADD COLUMN dispatch_issued_at timestamptz;
ALTER TABLE actions ADD COLUMN dispatch_not_after timestamptz;
ALTER TABLE actions ADD COLUMN dispatch_fencing_token bigint;
ALTER TABLE actions ADD CONSTRAINT dispatch_permit_complete CHECK(
 (dispatch_issued_at IS NULL AND dispatch_not_after IS NULL AND dispatch_fencing_token IS NULL)
 OR (dispatch_issued_at IS NOT NULL AND dispatch_not_after IS NOT NULL AND dispatch_fencing_token>0
 AND dispatch_not_after>dispatch_issued_at AND dispatch_not_after<=dispatch_issued_at+interval '2 seconds')
);
