-- +goose Up
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text NOT NULL UNIQUE,
    password_hash text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    id         text PRIMARY KEY,                       -- sha256 of the cookie token
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    csrf_token text NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_seen  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_user_idx ON sessions(user_id);

CREATE TABLE connections (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id          uuid NOT NULL REFERENCES users(id),
    kind              text NOT NULL CHECK (kind IN ('source', 'destination')),
    type              text NOT NULL,
    name              text NOT NULL,
    config            jsonb NOT NULL DEFAULT '{}'::jsonb,   -- non-secret fields only
    secret_ciphertext bytea,                                 -- sealed JSON object of secret fields
    last_test_status  text,
    last_test_report  jsonb,
    last_tested_at    timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX connections_owner_idx ON connections(owner_id, kind);

CREATE TABLE pipelines (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id        uuid NOT NULL REFERENCES users(id),
    name            text NOT NULL,
    source_id       uuid NOT NULL REFERENCES connections(id) ON DELETE RESTRICT,
    destination_id  uuid NOT NULL REFERENCES connections(id) ON DELETE RESTRICT,
    namespace       text NOT NULL,
    sync_interval_s integer NOT NULL DEFAULT 60,
    state           text NOT NULL DEFAULT 'created',
    desired_state   text NOT NULL DEFAULT 'stopped' CHECK (desired_state IN ('stopped', 'running', 'paused')),
    settings        jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX pipelines_owner_idx ON pipelines(owner_id);

CREATE TABLE pipeline_collections (
    pipeline_id        uuid NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    collection         text NOT NULL,                         -- "namespace.name" at the source
    enabled            boolean NOT NULL DEFAULT true,
    state              text NOT NULL DEFAULT 'created',
    raw_table          text NOT NULL,
    clean_table        text NOT NULL,
    generation         bigint NOT NULL DEFAULT 1,
    last_seq           bigint NOT NULL DEFAULT 0,
    merged_seq         bigint NOT NULL DEFAULT 0,
    snapshot_total_est bigint NOT NULL DEFAULT 0,
    snapshot_done      bigint NOT NULL DEFAULT 0,
    last_event_at      timestamptz,
    last_source_ts     timestamptz,
    last_commit_at     timestamptz,
    last_merge_at      timestamptz,
    last_error         text,
    state_message      text,
    resync_reason      text,
    requested_action   text,                                  -- resync | retry | maintenance, consumed by the worker
    failures           integer NOT NULL DEFAULT 0,            -- consecutive failures in the window
    schema             jsonb,                                 -- cached core.Schema for display
    updated_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (pipeline_id, collection)
);

CREATE TABLE field_mappings (
    pipeline_id    uuid NOT NULL,
    collection     text NOT NULL,
    source_field   text NOT NULL,
    column_name    text NOT NULL,
    iceberg_type   text NOT NULL,
    conflict_count bigint NOT NULL DEFAULT 0,
    reason         text,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (pipeline_id, collection, source_field),
    FOREIGN KEY (pipeline_id, collection) REFERENCES pipeline_collections(pipeline_id, collection) ON DELETE CASCADE
);

CREATE TABLE runs (
    id          bigserial PRIMARY KEY,
    pipeline_id uuid NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    collection  text NOT NULL DEFAULT '',
    kind        text NOT NULL CHECK (kind IN ('copy', 'stream', 'merge', 'maintenance', 'resync')),
    started_at  timestamptz NOT NULL DEFAULT now(),
    ended_at    timestamptz,
    status      text NOT NULL DEFAULT 'running',
    rows        bigint NOT NULL DEFAULT 0,
    bytes       bigint NOT NULL DEFAULT 0,
    error       text,
    details     jsonb
);
CREATE INDEX runs_pipeline_idx ON runs(pipeline_id, started_at DESC);

CREATE TABLE logs (
    id          bigserial PRIMARY KEY,
    pipeline_id uuid NOT NULL REFERENCES pipelines(id) ON DELETE CASCADE,
    collection  text NOT NULL DEFAULT '',
    level       text NOT NULL,
    message     text NOT NULL,
    fields      jsonb,
    at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX logs_pipeline_idx ON logs(pipeline_id, at DESC);

-- events per minute per collection, for sparklines and "events in the last hour"
CREATE TABLE event_counters (
    pipeline_id uuid NOT NULL,
    collection  text NOT NULL,
    minute      timestamptz NOT NULL,
    events      bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (pipeline_id, collection, minute),
    FOREIGN KEY (pipeline_id, collection) REFERENCES pipeline_collections(pipeline_id, collection) ON DELETE CASCADE
);

CREATE TABLE settings (
    key        text PRIMARY KEY,
    value      jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE query_history (
    id          bigserial PRIMARY KEY,
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    destination uuid,
    sql         text NOT NULL,
    duration_ms integer NOT NULL DEFAULT 0,
    rows        integer NOT NULL DEFAULT 0,
    error       text,
    at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX query_history_user_idx ON query_history(user_id, at DESC);

CREATE TABLE login_attempts (
    key        text PRIMARY KEY,   -- email or ip
    attempts   integer NOT NULL DEFAULT 0,
    blocked_until timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS login_attempts, query_history, settings, event_counters, logs, runs, field_mappings, pipeline_collections, pipelines, connections, sessions, users;
