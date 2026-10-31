# Executed verification

Date: 2026-10-31 (user timezone: Asia/Yekaterinburg). This records local execution, not a remote GitHub Actions run.

Environment: Linux amd64, host Go 1.26.8, Docker Engine 29.9.2, Compose 5.6.1. Docker builds use Go 1.26.10; PostgreSQL is 16.15 on Alpine 3.24, and MinIO is built from `RELEASE.2025-10-15T17-29-55Z`. Go dependency versions and checksums were preserved.

Local Go commands used `GOCACHE=/tmp/filestorage-go-build`, `GOMODCACHE=/tmp/filestorage-go-mod`, `GOPATH=/tmp/filestorage-go` to keep caches in a writable temporary location. Network downloads, local HTTP listener tests and Docker execution required the environment's approved sandbox escalation. No real cloud credentials were used.

| Executed command / check | Actual result |
| --- | --- |
| `go mod download` | PASS; pinned modules downloaded |
| `test -z "$(gofmt -l $(rg --files -g '*.go'))"` | PASS; includes new untracked Go files |
| `go vet ./...` | PASS |
| `go vet -tags=integration ./integration` | PASS |
| `go test -count=1 ./...` | PASS; 20 top-level unit tests in config/files/storage |
| `go test -race -count=1 ./...` | PASS; no reported races |
| `go build ./...` | PASS |
| `docker compose --env-file .env.example --file docker-compose.yml config --quiet` | PASS |
| `docker compose --env-file /dev/null --file docker-compose.test.yml config --quiet` | PASS |
| `bash -n scripts/test-integration.sh` | PASS |
| `shellcheck scripts/test-integration.sh` | PASS |
| `docker build --file deploy/minio.Dockerfile ...` | PASS; pinned MinIO source compiled into an image |
| `docker build --tag filestorage-portfolio-check .` | PASS on final application sources |
| `./scripts/test-integration.sh -race -v` | PASS; final run: 21 top-level tests + 29 subtests, `ok filestorage/integration 7.485s` |
| Main Compose runtime smoke in a unique disposable project | PASS; actual migrate service exit 0 before app startup; app healthy, `/ready` 200, upload 201, byte-identical binary download with correct length, delete 204 |
| Container inspection and SIGTERM in isolated Compose smoke | PASS; `Config.User=app:app`, read-only root filesystem; clean graceful exit 0 |
| `git diff --check` | PASS |

The initial synthetic S3 HEAD fixture omitted `Last-Modified`, which the real SDK validates. That fixture was corrected; both normal and race-enabled unit suites then passed. The initial sandbox denied downloading modules / opening local test listeners; these checks were rerun with the required environment permission and succeeded. These were not silently skipped.

Final temporary logs (local artifacts, not tracked): `/tmp/filestorage-unit-final.log`, `/tmp/filestorage-race-final.log`, `/tmp/filestorage-integration-final.log`, `/tmp/filestorage-docker-final.log`.

## Scenario evidence

| Required scenario | Verification |
| --- | --- |
| Upload / download identical bytes | Real HTTP + PostgreSQL + MinIO; binary payloads including NUL/0xff; exact 20 MiB default limit verified with SHA-256 |
| Empty file | HTTP 400; no available metadata/object created |
| Exact configured limit | 1 MiB and default 20 MiB HTTP cases pass |
| Excess file/body size | HTTP 413; total-body, streamed epilogue and file-byte cases |
| Invalid multipart | Missing boundary/field, truncated body, wrong media type and extra parts rejected |
| Unicode, quotes, special characters, CRLF | Safe download headers roundtrip; path basenames normalized; encoded CRLF/control characters rejected |
| Delete / repeat / two concurrent deletes | Real-store HTTP 204; no available row/object remains |
| Cancel during Upload | Fault wrapper cancels the request around the real S3 operation; separate live compensation context verified; unit tests cover cancellation as well |
| SQL failure after successful S3 PUT | Real object written, activation fault injected; hidden intent cleaned; unknown successful activation preserves ready object |
| MinIO Upload failure | Before-write and lost-acknowledgement-after-write wrappers around real S3 |
| SQL Delete failure | Before intent, ambiguous committed intent and final-update errors; real-store recovery verified |
| MinIO Delete failure | Failure before remove and lost acknowledgement after actual remove; repeat/recovery verified |
| Intermediate-state recovery | Seeded durable uploading/deleting/deleted records with/without real objects; new service reconciles them |
| Late PUT after cleanup | Real object written at retained tombstone key; subsequent sweep removes it |
| Concurrent recovery | 64 real SQL rows claimed in disjoint 32-row sets; concurrent cleanup and active-upload protection |
| Dependency unavailability | Unreachable real network endpoints, failed PostgreSQL operations and an unavailable MinIO gateway; bounded checks return errors/503 |
| Existing-data migration | Legacy row seeded before 002; bytes and UUID.extension key containing a literal backslash survive; unsafe downgrade is refused |
| Process restart | Compiled application starts, uploads, exits cleanly on SIGTERM, restarts against the same stores, downloads identical bytes and deletes successfully |
| Initial S3 read failure | Local protocol fixture allows HEAD but denies GET; adapter returns error before handing a response body to HTTP |
| Missing/size-changed S3 object | Real store tests return 503 before HTTP 200 |
| Temporary files / resource bounds | Unit tests check cleanup on success/failure/malformed requests, body limits and upload saturation; Unix immediate unlink protects killed-process spools |

## Limits of the evidence

- GitHub Actions was authored and its component commands ran locally. The workflow itself was not pushed or executed remotely, as requested.
- Tests do not physically sever a PostgreSQL commit acknowledgement, power off a host, or kill a process at every instruction boundary. Wrappers model precise before/after-commit outcomes against real stores; durable-state tests seed the corresponding crash windows. The restart/SIGTERM test launches actual processes.
- The five-second claim-query deadline is implemented and statically reviewed; a blackholed PostgreSQL TCP connection was not injected in an end-to-end test.
- The maximum tested file is 20 MiB. The 5 GiB protocol/config ceiling, disk-full behavior, sustained hostile traffic, and multi-hour soak/load tests were not exercised.
- amd64 Linux was tested. Other CPU architectures and Windows were not executed.
- Versioned/locked buckets, external object mutation of the same size, production TLS/gateways and recovery of pre-existing orphan objects were not validated.
- User containers, databases and volumes were not used for testing or deleted. Each integration run and the main-Compose smoke used a unique disposable Compose project and cleaned only that project's containers, network and volumes. Local build images/caches remain available.
