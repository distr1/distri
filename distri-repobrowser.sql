CREATE TABLE upstream_status (
    package text NOT NULL,
    upstream_version text,
    last_reachable timestamp without time zone,
    unreachable boolean DEFAULT true NOT NULL
);

ALTER TABLE ONLY upstream_status
    ADD CONSTRAINT upstream_status_pkey PRIMARY KEY (package);
