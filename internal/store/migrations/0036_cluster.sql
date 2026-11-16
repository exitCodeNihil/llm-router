-- More than one control plane at once. Each registers here every second, and
-- they share what they count through cp_rate_log.
CREATE TABLE cp_replicas (
    id         text PRIMARY KEY,           -- random per process
    started_at timestamptz NOT NULL DEFAULT now(),
    last_seen  timestamptz NOT NULL DEFAULT now(),
    version    text NOT NULL DEFAULT '',
    key_fp     text NOT NULL DEFAULT '',   -- fingerprint of its current encryption key
    state      jsonb NOT NULL DEFAULT '{}' -- live metrics and the gateways syncing with it
);

-- What each replica counted in the last second: requests and tokens per scope
-- and minute for rate limits, and which scopes it committed spend for, so the
-- others read those totals again. Rows are numbered by the replica that wrote
-- them (one writer each, so its numbers are in commit order) and are deleted
-- after three minutes. Unlogged: after a crash the counts of the last two
-- minutes are gone, as they are when a control plane restarts.
CREATE UNLOGGED TABLE cp_rate_log (
    replica  text NOT NULL,
    seq      bigint NOT NULL,
    ts       timestamptz NOT NULL DEFAULT now(),
    scope    text NOT NULL,
    minute   bigint NOT NULL DEFAULT 0,
    requests bigint NOT NULL DEFAULT 0,
    tokens   bigint NOT NULL DEFAULT 0,
    spend    boolean NOT NULL DEFAULT false,
    PRIMARY KEY (replica, seq)
);
CREATE INDEX cp_rate_log_ts ON cp_rate_log (ts);
