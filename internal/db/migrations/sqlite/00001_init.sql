-- +goose Up
CREATE TABLE machines (
    id            TEXT PRIMARY KEY,
    smbios_uuid   TEXT NOT NULL,
    junk          INTEGER NOT NULL DEFAULT 0 CHECK (junk IN (0, 1)),
    serial        TEXT NOT NULL,
    product       TEXT NOT NULL,
    manufacturer  TEXT NOT NULL DEFAULT '',
    arch          TEXT NOT NULL CHECK (arch IN ('amd64', 'arm64')),
    state         TEXT NOT NULL CHECK (state IN ('discovered', 'local-only', 'assigned', 'installing', 'install-unconfirmed', 'installed', 'rejected', 'orphaned', 'cordoned')),
    prior_state   TEXT CHECK (prior_state IS NULL OR prior_state IN ('discovered', 'local-only', 'assigned', 'installing', 'install-unconfirmed', 'installed', 'rejected', 'orphaned')),
    orphaned_from TEXT CHECK (orphaned_from IS NULL OR orphaned_from IN ('assigned', 'installing', 'install-unconfirmed', 'installed')),
    cluster_id    TEXT,
    host_name     TEXT,
    created_at    TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    -- A lifecycle shape Next can produce: cordoned remembers a prior state, and only an
    -- orphaned machine, or one cordoned over orphaned, remembers the state it was orphaned from;
    -- cordoned over orphaned always does.
    CHECK ((state = 'cordoned') = (prior_state IS NOT NULL)),
    CHECK (orphaned_from IS NULL OR state = 'orphaned' OR (state = 'cordoned' AND prior_state = 'orphaned')),
    CHECK (state <> 'orphaned' OR orphaned_from IS NOT NULL),
    CHECK (state <> 'cordoned' OR prior_state <> 'orphaned' OR orphaned_from IS NOT NULL)
) STRICT;
CREATE UNIQUE INDEX machines_valid_uuid ON machines (smbios_uuid) WHERE junk = 0;
CREATE INDEX machines_uuid ON machines (smbios_uuid);
CREATE TABLE machine_macs (
    mac        TEXT PRIMARY KEY,
    machine_id TEXT NOT NULL REFERENCES machines (id) ON DELETE CASCADE,
    seen_at    TEXT NOT NULL
) STRICT;
CREATE INDEX machine_macs_machine ON machine_macs (machine_id);
CREATE TABLE inventory_reports (
    id          INTEGER PRIMARY KEY,
    machine_id  TEXT NOT NULL REFERENCES machines (id) ON DELETE CASCADE,
    source      TEXT NOT NULL CHECK (source IN ('ipxe')),
    facts       TEXT NOT NULL,
    received_at TEXT NOT NULL
) STRICT;
CREATE INDEX inventory_reports_machine ON inventory_reports (machine_id, id);
CREATE TABLE boot_events (
    id           INTEGER PRIMARY KEY,
    machine_id   TEXT NOT NULL REFERENCES machines (id) ON DELETE CASCADE,
    at           TEXT NOT NULL,
    decision_row INTEGER NOT NULL CHECK (decision_row BETWEEN 1 AND 10),
    action       TEXT NOT NULL,
    reason       TEXT NOT NULL DEFAULT '',
    from_state   TEXT,
    to_state     TEXT
) STRICT;
CREATE INDEX boot_events_machine ON boot_events (machine_id, id);
CREATE TABLE audit_events (
    id         INTEGER PRIMARY KEY,
    at         TEXT NOT NULL,
    actor      TEXT NOT NULL,
    action     TEXT NOT NULL,
    machine_id TEXT,
    detail     TEXT NOT NULL DEFAULT '{}'
) STRICT;
CREATE INDEX audit_events_at ON audit_events (at);
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;
INSERT INTO settings (key, value) VALUES ('unknown_action', 'local');

-- +goose Down
DROP TABLE settings;
DROP TABLE audit_events;
DROP TABLE boot_events;
DROP TABLE inventory_reports;
DROP TABLE machine_macs;
DROP TABLE machines;
