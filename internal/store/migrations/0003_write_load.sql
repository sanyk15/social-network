-- Lab table for the replication homework: write load with an easy success
-- count. It is deliberately small and has no secondary indexes.
CREATE TABLE IF NOT EXISTS write_load (
    id         BIGSERIAL   PRIMARY KEY,
    payload    TEXT        NOT NULL DEFAULT 'k6',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
