-- +goose Up
ALTER TABLE machines ADD COLUMN boot_next TEXT, ADD COLUMN boot_next_set_at TIMESTAMPTZ, ADD COLUMN boot_next_mac TEXT, ADD CONSTRAINT machines_boot_next_pair CHECK ((boot_next IS NULL) = (boot_next_set_at IS NULL)), ADD CONSTRAINT machines_boot_next_mac_needs_flag CHECK (boot_next_mac IS NULL OR boot_next IS NOT NULL);
CREATE TABLE utility_cache (
    entry           TEXT NOT NULL,
    arch            TEXT NOT NULL CHECK (arch IN ('amd64', 'arm64')),
    pin             TEXT NOT NULL,
    state           TEXT NOT NULL CHECK (state IN ('wanted', 'fetching', 'ready', 'failed', 'broken')),
    attempts        INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ,
    priority        INTEGER NOT NULL DEFAULT 0 CHECK (priority IN (0, 1)),
    bytes_done      BIGINT NOT NULL DEFAULT 0 CHECK (bytes_done >= 0),
    bytes_total     BIGINT NOT NULL DEFAULT 0 CHECK (bytes_total >= 0),
    last_error      TEXT NOT NULL DEFAULT '',
    updated_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (entry, arch),
    CHECK (state <> 'failed' OR next_attempt_at IS NOT NULL)
);

-- +goose Down
DROP TABLE utility_cache;
ALTER TABLE machines DROP CONSTRAINT machines_boot_next_mac_needs_flag, DROP CONSTRAINT machines_boot_next_pair, DROP COLUMN boot_next_mac, DROP COLUMN boot_next_set_at, DROP COLUMN boot_next;
