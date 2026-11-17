-- +goose Up
ALTER TABLE machines ADD COLUMN boot_next TEXT;
ALTER TABLE machines ADD COLUMN boot_next_set_at TEXT CHECK ((boot_next IS NULL) = (boot_next_set_at IS NULL));
CREATE TABLE utility_cache (
    entry           TEXT NOT NULL,
    arch            TEXT NOT NULL CHECK (arch IN ('amd64', 'arm64')),
    pin             TEXT NOT NULL,
    state           TEXT NOT NULL CHECK (state IN ('wanted', 'fetching', 'ready', 'failed', 'broken')),
    attempts        INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TEXT,
    priority        INTEGER NOT NULL DEFAULT 0 CHECK (priority IN (0, 1)),
    bytes_done      INTEGER NOT NULL DEFAULT 0 CHECK (bytes_done >= 0),
    bytes_total     INTEGER NOT NULL DEFAULT 0 CHECK (bytes_total >= 0),
    last_error      TEXT NOT NULL DEFAULT '',
    updated_at      TEXT NOT NULL,
    PRIMARY KEY (entry, arch),
    CHECK (state <> 'failed' OR next_attempt_at IS NOT NULL)
) STRICT;

-- +goose Down
DROP TABLE utility_cache;
ALTER TABLE machines DROP COLUMN boot_next_set_at;
ALTER TABLE machines DROP COLUMN boot_next;
