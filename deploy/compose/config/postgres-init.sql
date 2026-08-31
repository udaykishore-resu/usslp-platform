-- USSLP — PostgreSQL 16 bootstrap for the prod-like compose profile.
--
-- IMPORTANT: nothing in the Go tree connects to PostgreSQL today. The event
-- store (platform/pkg/eventstore) and every read model are built on
-- platform/pkg/kvstore, an embedded LSM store. This script provisions the
-- databases, roles and extensions the documented production port expects, so
-- that the adapter — when it is written behind the existing repository
-- interfaces — has a target that already matches what Terraform provisions in
-- deploy/terraform/modules/aurora.
--
-- Nothing here invents a schema for tables that do not exist. It creates the
-- databases and the two roles the platform's access model needs, and stops.

\set ON_ERROR_STOP on

-- ---------------------------------------------------------------------------
-- Databases
--
-- One database per bounded context rather than one shared database with
-- schemas: the services are deployed and scaled independently, and a shared
-- database makes a slow query in analytics a connection-pool outage on the
-- price path.
-- ---------------------------------------------------------------------------
CREATE DATABASE usslp_label_service;
CREATE DATABASE usslp_device_registry;
CREATE DATABASE usslp_ota_service;
CREATE DATABASE usslp_uig;

-- ---------------------------------------------------------------------------
-- Roles
--
-- usslp_app owns the schema and writes; usslp_readonly is what a read replica
-- and an analyst get. Passwords here are compose-local; in production these are
-- IAM database authentication tokens issued through IRSA, which is why
-- deploy/terraform/modules/aurora sets iam_database_authentication_enabled.
-- ---------------------------------------------------------------------------
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'usslp_app') THEN
    CREATE ROLE usslp_app LOGIN PASSWORD 'usslp-change-me';
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'usslp_readonly') THEN
    CREATE ROLE usslp_readonly LOGIN PASSWORD 'usslp-change-me';
  END IF;
  -- Debezium reads the logical replication slot to mirror the relational
  -- stores into Kafka. wal_level=logical in the compose command line is what
  -- makes that possible.
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'usslp_replication') THEN
    CREATE ROLE usslp_replication LOGIN REPLICATION PASSWORD 'usslp-change-me';
  END IF;
END
$$;

GRANT ALL PRIVILEGES ON DATABASE usslp_label_service   TO usslp_app;
GRANT ALL PRIVILEGES ON DATABASE usslp_device_registry TO usslp_app;
GRANT ALL PRIVILEGES ON DATABASE usslp_ota_service     TO usslp_app;
GRANT ALL PRIVILEGES ON DATABASE usslp_uig             TO usslp_app;

GRANT CONNECT ON DATABASE usslp_label_service   TO usslp_readonly;
GRANT CONNECT ON DATABASE usslp_device_registry TO usslp_readonly;
GRANT CONNECT ON DATABASE usslp_ota_service     TO usslp_readonly;
GRANT CONNECT ON DATABASE usslp_uig             TO usslp_readonly;

-- pgcrypto for gen_random_uuid(); the platform's own identifiers come from
-- canon, but a surrogate key on an outbox table wants one.
\connect usslp_label_service
CREATE EXTENSION IF NOT EXISTS pgcrypto;
\connect usslp_device_registry
CREATE EXTENSION IF NOT EXISTS pgcrypto;
\connect usslp_ota_service
CREATE EXTENSION IF NOT EXISTS pgcrypto;
\connect usslp_uig
CREATE EXTENSION IF NOT EXISTS pgcrypto;
