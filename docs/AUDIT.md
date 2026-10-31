# FileStorage audit

Audit baseline: original repository on 2026-10-31, before implementation. Scope: all Go sources, both original SQL migrations, Dockerfile, Compose, configuration, ignore files and README. No pre-existing tests or CI were present.

| Priority | Finding | Consequence | Planned correction |
| --- | --- | --- | --- |
| CRITICAL | Upload writes S3 before creating metadata; compensation uses cancelled request context and ignores errors | Process crash / failed SQL / ambiguous acknowledgement leaves untracked objects | Persist upload intent first; conditional state transitions; independent bounded compensation; durable reconciliation |
| CRITICAL | Delete removes S3 before updating SQL | Database failures expose metadata for deleted content; concurrent deletes disagree | Persist deletion before touching S3; idempotent deletion; retain cleanup tombstones |
| HIGH | No state machine, recovery worker or handling of unknown commit outcomes | Restart cannot repair intermediate operations; blind compensation may delete a successfully committed upload | Guard transitions in SQL and periodically reconcile pending operations; do not compensate a ready upload |
| HIGH | Unbounded HTTP body and multipart disk usage; 32 MiB in-memory parser; temporary multipart files not removed explicitly | Memory/disk exhaustion, especially public unauthenticated uploads | Strict total-body and file limits; bounded streaming spool; cleanup on all exits; bounded parallel uploads |
| HIGH | Client filename extension becomes part of object key; manually assembled Content-Disposition | Unsafe keys, malformed headers and Unicode/control-character problems | UUID-only keys; validate and normalize names; standard MIME header formatting |
| HIGH | S3 GetObject is lazy; stat does not guarantee the subsequent GET succeeds | HTTP 200 can precede first read failure | Prefetch initial bytes, compare sizes before sending headers, abort interrupted transfers |
| HIGH | PostgreSQL and MinIO ports published on all interfaces with passwords in Compose; runtime is root | Exposed dependencies and poor secret separation | Private dependency network, loopback API, explicit local credentials, nonroot image |
| HIGH | App does not depend on migration completion; readiness ignores missing bucket and schema | App accepts requests against incomplete dependencies | Migration completion dependency; schema/bucket-aware readiness |
| MEDIUM | Fixed 30-second read/write timeouts and 5-second shutdown; server goroutine shares err and calls os.Exit | Large transfers fail; shutdown drops requests; possible data race | Explicit transfer deadlines, signal lifecycle, worker cancellation and graceful forced-close fallback |
| MEDIUM | Invalid config silently defaults; MB multiplication can overflow | Incorrect TLS and size limits are accepted | Fail-fast configuration validation without printing secret values |
| MEDIUM | Invalid UUID produces database error; repeated deletion returns 404 | Inconsistent client behavior and unnecessary internal errors | Validate IDs and return deterministic 400 / idempotent 204 |
| MEDIUM | amd64 forced; empty .dockerignore; mutable latest MinIO; no test isolation | Nonportable build, secret build-context leaks, irreproducible startup | Native multi-stage builds; narrow build context; pinned dependency builds; dedicated test Compose project |
| MEDIUM | No tests, race checks or CI; README only describes happy paths | Recovery and operational behavior are unverified | Unit fault tests, real PostgreSQL/MinIO integration suite, GitHub Actions, English operational documentation |

Implementation order: durable schema and transitions; HTTP/storage safeguards; lifecycle/configuration; isolated infrastructure; fault/integration tests and CI; documentation. Existing routes and user volume names are retained. No distributed transaction or additional infrastructure is required.

## Consistency acceptance criteria

Only `ready` rows are public. An intent must be durable before a new S3 object is written. SQL transitions must prevent cleanup from deleting a row that already became ready. Failed or abandoned uploads and deletions must remain discoverable after restart. Tombstones must remain available for repeated cleanup of S3 writes whose acknowledgement was lost or which completed late. Existing metadata is migrated to `ready` without replacing object keys or removing data.

The mechanism provides eventual repair for application-managed objects, not an atomic transaction between PostgreSQL and S3. Objects orphaned by the old implementation cannot be reliably attributed and must not be blindly deleted. Out-of-band modification, bucket versioning and external deletion remain operational limitations.

## Verification record

Actual commands and results are recorded in `docs/VERIFICATION.md` after execution. An unexecuted or skipped check is not a pass.

## Resolution

All listed CRITICAL/HIGH findings have implementation coverage: durable lifecycle and conditional SQL (`internal/files/{service,repository}.go`, migration 002); bounded multipart/name/header handling (`internal/files/handler.go`); eager S3 initial read and size verification (`internal/storage/minio.go`); private dependency networking and migration startup order (`docker-compose.yml`); nonroot native image (`Dockerfile`); readiness and orderly worker/server shutdown (`cmd/app/main.go`). MEDIUM configuration, UUID, test isolation, CI and documentation findings were also addressed.

Independent review additionally identified and corrected an unbounded worker claim query, misleading download-timeout wording, and rejection of a backslash-bearing legacy UUID.extension key. The latter is now covered against real MinIO. See `docs/VERIFICATION.md` for actual tests and unverified scenarios. Remaining scale, authentication, bucket-versioning and old-orphan limitations are explicit in README; “resolved” does not imply production certification or atomicity across stores.
