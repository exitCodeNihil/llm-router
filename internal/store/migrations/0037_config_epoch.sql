-- Names the run of config versions gateways count in. A control plane writes
-- a new one when it finds the version went backwards: the database was
-- restored to an earlier state in place, where Postgres' own identity (which
-- the epoch gateways see also carries) does not change.
INSERT INTO settings (key, value) VALUES ('config_epoch', to_jsonb(md5(random()::text || clock_timestamp()::text)))
ON CONFLICT (key) DO NOTHING;
