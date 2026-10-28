-- SPDX-License-Identifier: Elastic-2.0
ALTER TABLE organizations ADD COLUMN pending_task_limit integer NOT NULL DEFAULT 1000 CHECK(pending_task_limit BETWEEN 1 AND 100000);
