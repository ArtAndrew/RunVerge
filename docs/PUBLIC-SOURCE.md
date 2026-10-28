# Public source contents

This repository contains product source, API contracts, SDKs, examples, user/operator instructions and license files. Internal plans, ADRs, work orders, progress tracking, private reviews, raw logs, private configuration, downloaded dependencies and model weights are not distributed. The published Git history starts with this reviewed public snapshot; private development history is retained locally.

Two non-secret schema fixtures are retained under docs/evidence for existing source tests. They describe the trusted database schema and contain no application data. Other retained JSON test fixtures are synthetic or historical test inputs; personal path prefixes have been removed.

Release assets identify their exact source inventory and checksums. Validation distinguishes a previously verified PostgreSQL baseline from later targeted interface/documentation checks, agent-led browser verification and independent user testing. See VALIDATION-alpha.1.md for the actual checks and limits.
