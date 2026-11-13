-- Deleting a template clears it from its workspaces.
CREATE INDEX workspaces_template ON workspaces (template_id);

-- Tables whose rows are updated in place many times a second. Free space on
-- each page lets an update stay on its page, and vacuum starts after 2% of the
-- rows have changed instead of the default 20%.
ALTER TABLE spend_counters SET (fillfactor = 90, autovacuum_vacuum_scale_factor = 0.02, autovacuum_analyze_scale_factor = 0.02);
ALTER TABLE spend_totals SET (fillfactor = 90, autovacuum_vacuum_scale_factor = 0.02, autovacuum_analyze_scale_factor = 0.02);

-- A partitioned table takes no storage parameters itself: set them on each
-- partition. New partitions are created with the same settings.
DO $$
DECLARE part regclass;
BEGIN
    FOR part IN
        SELECT inhrelid::regclass FROM pg_inherits
        WHERE inhparent IN ('usage_hourly'::regclass, 'usage_daily'::regclass)
    LOOP
        EXECUTE format('ALTER TABLE %s SET (fillfactor = 90, autovacuum_vacuum_scale_factor = 0.02, autovacuum_analyze_scale_factor = 0.02)', part);
    END LOOP;
END $$;
