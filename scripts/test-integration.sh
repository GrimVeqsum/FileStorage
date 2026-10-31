#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

# mktemp supplies a collision-resistant suffix. Never use COMPOSE_PROJECT_NAME,
# the user's .env, or the normal Compose file when creating/cleaning test data.
test_marker=$(mktemp -d "${TMPDIR:-/tmp}/filestorage-it-XXXXXXXX")
test_project="filestorage-it-$(basename "$test_marker" | tr '[:upper:]' '[:lower:]')"
compose=(docker compose --env-file /dev/null --project-name "$test_project" --file docker-compose.test.yml)

cleanup() {
  test_status=$?
  trap - EXIT INT TERM
  if ((test_status != 0)); then
    "${compose[@]}" logs --no-color --tail=100 || true
  fi
  # -v is deliberate here: this project contains only this run's disposable data.
  "${compose[@]}" down --volumes --remove-orphans || true
  rmdir "$test_marker"
  exit "$test_status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

"${compose[@]}" up --build --detach --wait --wait-timeout 180
postgres_address=$("${compose[@]}" port postgres 5432)
minio_address=$("${compose[@]}" port minio 9000)

export TEST_DATABASE_URL="postgres://filestorage_test:integration-only-password@${postgres_address}/filestorage_test?sslmode=disable"
export TEST_MINIO_ENDPOINT="$minio_address"
export TEST_MINIO_ACCESS_KEY=integration-only-key
export TEST_MINIO_SECRET_KEY=integration-only-secret
export TEST_MINIO_BUCKET=filestorage-integration

go test -tags=integration -count=1 -timeout=10m ./integration "$@"
