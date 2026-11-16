-- A gateway group can be held to its region: its gateways then get only the
-- deployments labelled with it, not the others as a fallback.
ALTER TABLE gateway_groups ADD COLUMN strict_region boolean NOT NULL DEFAULT false;
