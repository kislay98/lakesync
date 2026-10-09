#!/usr/bin/env bash
# Runs the Playwright suite against a fresh lakesync instance (empty database) so the
# first-run flow is exercised. Needs `make infra` and `make build`.
set -euo pipefail
cd "$(dirname "$0")/.."
PORT=${LAKESYNC_UI_E2E_PORT:-18090}
BIN=${LAKESYNC_BIN:-./bin/lakesync}
MONGO=${SEED_MONGODB_URI:-mongodb://localhost:27017/?directConnection=true}
DB="lakesync_ui_e2e_$(date +%s)"
PG_HOST=${LAKESYNC_E2E_PG_HOST:-localhost:5432}
docker exec lakesync-postgres-1 psql -U lakesync -d lakesync -qc "CREATE DATABASE $DB" >/dev/null
cleanup() {
  kill "${PID:-0}" 2>/dev/null || true
  docker exec lakesync-postgres-1 psql -U lakesync -d lakesync -qc "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null 2>&1 || true
}
trap cleanup EXIT
DATABASE_URL="postgres://lakesync:lakesync@$PG_HOST/$DB?sslmode=disable" LISTEN_ADDR=":$PORT" DEV_MODE=true DISABLE_IP_LOOKUP=true \
  APP_SECRET_KEY=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  ADMIN_EMAIL=admin@example.com ADMIN_PASSWORD=changeme-now ALLOW_PRIVATE_ENDPOINTS=true \
  DEFAULT_SYNC_INTERVAL_S=10 MIN_SYNC_INTERVAL_S=5 LOG_FORMAT=json \
  "$BIN" serve > "/tmp/lakesync-ui-e2e-$PORT.log" 2>&1 &
PID=$!
for i in $(seq 1 60); do curl -sf "http://localhost:$PORT/readyz" >/dev/null && break; sleep 1; done
export LAKESYNC_URL="http://localhost:$PORT"
# seed a small dataset so the viewer/history specs have data once the setup spec created a pipeline
"$BIN" seed --uri "$MONGO" --drop --initial customers=300,products=50,orders=500,events=800 --quiet >/dev/null
cd web && npx playwright test --project=e2e "$@"
