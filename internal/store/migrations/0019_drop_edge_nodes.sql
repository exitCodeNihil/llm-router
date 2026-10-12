-- Edge nodes are gone: data-plane gateways join through gateway groups.
-- usage_events keeps which gateway instance served each request.
DROP TABLE edge_nodes;
ALTER TABLE usage_events RENAME COLUMN edge_node_id TO gateway_id;
