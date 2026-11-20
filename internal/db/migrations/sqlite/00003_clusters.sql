-- +goose Up
ALTER TABLE machines ADD COLUMN identity_observed INTEGER NOT NULL DEFAULT 1 CHECK (identity_observed IN (0, 1));
ALTER TABLE machines ADD COLUMN talos_schematic TEXT;
ALTER TABLE machines ADD COLUMN talos_version TEXT CHECK ((talos_schematic IS NULL) = (talos_version IS NULL));
CREATE UNIQUE INDEX machines_binding ON machines (cluster_id, host_name) WHERE cluster_id IS NOT NULL AND host_name IS NOT NULL;
CREATE TABLE clusters (
    id                     TEXT PRIMARY KEY,
    name                   TEXT NOT NULL UNIQUE,
    repo_url               TEXT NOT NULL,
    ref                    TEXT NOT NULL,
    subpath                TEXT NOT NULL,
    username               TEXT NOT NULL DEFAULT '',
    credential_ref         TEXT NOT NULL DEFAULT '',
    credential_sealed      BLOB,
    revision               INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
    last_fetch_at          TEXT,
    fetch_error_sealed     BLOB,
    last_head_tree         TEXT,
    processing_tree        TEXT,
    processing_revision    INTEGER,
    failed_tree            TEXT,
    failed_error_sealed    BLOB,
    approved_pair_id       TEXT,
    pending_pair_id        TEXT,
    refresh_requested      INTEGER NOT NULL DEFAULT 0 CHECK (refresh_requested IN (0, 1)),
    refresh_incomplete     INTEGER NOT NULL DEFAULT 0 CHECK (refresh_incomplete IN (0, 1)),
    last_refresh_at        TEXT,
    last_daily_at          TEXT,
    attempts               INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at        TEXT,
    rate_limited_until     TEXT,
    last_call_error_sealed BLOB,
    created_at             TEXT NOT NULL,
    updated_at             TEXT NOT NULL,
    CHECK ((processing_tree IS NULL) = (processing_revision IS NULL))
) STRICT;
CREATE TABLE secret_versions (
    id            TEXT PRIMARY KEY,
    cluster_id    TEXT NOT NULL REFERENCES clusters (id) ON DELETE CASCADE,
    values_sealed BLOB NOT NULL,
    created_at    TEXT NOT NULL
) STRICT;
CREATE TABLE cluster_pairs (
    id                TEXT PRIMARY KEY,
    cluster_id        TEXT NOT NULL REFERENCES clusters (id) ON DELETE CASCADE,
    tree_hash         TEXT NOT NULL,
    secret_version_id TEXT NOT NULL REFERENCES secret_versions (id),
    talos_version     TEXT NOT NULL,
    created_at        TEXT NOT NULL,
    approved_at       TEXT,
    approved_by       TEXT
) STRICT;
CREATE UNIQUE INDEX cluster_pairs_version ON cluster_pairs (secret_version_id);
CREATE TABLE host_renders (
    pair_id               TEXT NOT NULL REFERENCES cluster_pairs (id) ON DELETE CASCADE,
    host_name             TEXT NOT NULL,
    role                  TEXT NOT NULL,
    ip                    TEXT NOT NULL,
    config_hash           TEXT NOT NULL,
    display_hash          TEXT NOT NULL,
    redacted_sealed       BLOB NOT NULL,
    schematic_id          TEXT NOT NULL,
    schematic_body        BLOB,
    talos_version         TEXT NOT NULL,
    drift                 INTEGER NOT NULL DEFAULT 0 CHECK (drift IN (0, 1)),
    drift_config_hash     TEXT,
    drift_display_hash    TEXT,
    drift_redacted_sealed BLOB,
    PRIMARY KEY (pair_id, host_name),
    CHECK ((drift = 1) = (drift_config_hash IS NOT NULL))
) STRICT;
CREATE TABLE tickets (
    id                TEXT PRIMARY KEY,
    machine_id        TEXT NOT NULL REFERENCES machines (id) ON DELETE CASCADE,
    token_sha256      TEXT NOT NULL UNIQUE,
    cluster_id        TEXT NOT NULL,
    host_name         TEXT NOT NULL,
    pair_id           TEXT NOT NULL,
    secret_version_id TEXT NOT NULL,
    minted_at         TEXT NOT NULL,
    expires_at        TEXT NOT NULL,
    requested_at      TEXT,
    consumed_at       TEXT,
    revoked_at        TEXT
) STRICT;
CREATE UNIQUE INDEX tickets_live ON tickets (machine_id) WHERE consumed_at IS NULL AND revoked_at IS NULL;
CREATE TABLE talos_cache (
    schematic       TEXT NOT NULL,
    version         TEXT NOT NULL,
    arch            TEXT NOT NULL CHECK (arch IN ('amd64', 'arm64')),
    state           TEXT NOT NULL CHECK (state IN ('wanted', 'fetching', 'ready', 'failed', 'broken')),
    body            BLOB,
    attempts        INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TEXT,
    bytes_done      INTEGER NOT NULL DEFAULT 0 CHECK (bytes_done >= 0),
    bytes_total     INTEGER NOT NULL DEFAULT 0 CHECK (bytes_total >= 0),
    last_error      TEXT NOT NULL DEFAULT '',
    assets          TEXT NOT NULL DEFAULT '{}',
    updated_at      TEXT NOT NULL,
    PRIMARY KEY (schematic, version, arch),
    CHECK (state <> 'failed' OR next_attempt_at IS NOT NULL)
) STRICT;

-- +goose Down
DROP TABLE talos_cache;
DROP INDEX tickets_live;
DROP TABLE tickets;
DROP TABLE host_renders;
DROP INDEX cluster_pairs_version;
DROP TABLE cluster_pairs;
DROP TABLE secret_versions;
DROP TABLE clusters;
DROP INDEX machines_binding;
ALTER TABLE machines DROP COLUMN talos_version;
ALTER TABLE machines DROP COLUMN talos_schematic;
ALTER TABLE machines DROP COLUMN identity_observed;
