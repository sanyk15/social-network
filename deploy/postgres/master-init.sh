#!/bin/bash
# Runs once during the first initialization of the master data directory.
# Creates the streaming-replication role and allows replication connections
# from the Docker network.
set -euo pipefail

: "${REPLICATION_USER:=replicator}"
: "${REPLICATION_PASSWORD:=replpass}"

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<EOSQL
DO \$\$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '${REPLICATION_USER}') THEN
        CREATE ROLE ${REPLICATION_USER} WITH REPLICATION LOGIN PASSWORD '${REPLICATION_PASSWORD}';
    END IF;
END
\$\$;
EOSQL

{
    echo "host replication ${REPLICATION_USER} 0.0.0.0/0 scram-sha-256"
    echo "host replication ${REPLICATION_USER} ::/0 scram-sha-256"
} >> "$PGDATA/pg_hba.conf"

echo "replication role ${REPLICATION_USER} is ready"
