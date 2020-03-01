-- sudo -u postgres createdb -E utf8 --owner michael distri
-- psql distri < upstream_status.schema

CREATE TABLE IF NOT EXISTS upstream_status (
  package TEXT NOT NULL PRIMARY KEY, -- like in distri/pkgs/, e.g. i3lock
  upstream_version TEXT NULL,
  last_reachable TIMESTAMP NULL,
  unreachable BOOLEAN DEFAULT true NOT NULL
);
