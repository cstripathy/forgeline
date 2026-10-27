CREATE TABLE runs (
    seq           bigserial   NOT NULL UNIQUE,
    id            text        PRIMARY KEY,
    pipeline_name text        NOT NULL,
    pipeline_yaml text        NOT NULL,
    status        text        NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'canceled')),
    reason        text        NOT NULL DEFAULT '',
    source_repo   text        NOT NULL DEFAULT '',
    source_ref    text        NOT NULL DEFAULT '',
    source_sha    text        NOT NULL DEFAULT '',
    trigger       text        NOT NULL CHECK (trigger IN ('api', 'github')),
    created_at    timestamptz NOT NULL,
    started_at    timestamptz,
    finished_at   timestamptz
);
CREATE INDEX runs_status_seq ON runs (status, seq);

CREATE TABLE steps (
    run_id      text        NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    step_id     text        NOT NULL,
    ord         integer     NOT NULL,
    status      text        NOT NULL CHECK (status IN ('pending', 'running', 'succeeded', 'cached', 'failed', 'skipped', 'canceled')),
    exit_code   integer     NOT NULL DEFAULT 0,
    error       text        NOT NULL DEFAULT '',
    started_at  timestamptz,
    finished_at timestamptz,
    PRIMARY KEY (run_id, step_id)
);

CREATE TABLE log_chunks (
    run_id     text        NOT NULL,
    step_id    text        NOT NULL,
    seq        bigint      NOT NULL,
    data       bytea       NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (run_id, step_id, seq),
    FOREIGN KEY (run_id, step_id) REFERENCES steps (run_id, step_id) ON DELETE CASCADE
);
