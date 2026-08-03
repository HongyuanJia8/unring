SET client_min_messages = warning;

DROP TABLE IF EXISTS demo_ddl_rollback;
DROP TABLE IF EXISTS demo_events;
DROP TABLE IF EXISTS demo_projects;
DROP TABLE IF EXISTS demo_accounts;

CREATE TABLE demo_accounts (
    id integer PRIMARY KEY,
    name text NOT NULL
);

CREATE TABLE demo_projects (
    id integer PRIMARY KEY,
    account_id integer NOT NULL REFERENCES demo_accounts(id) ON DELETE CASCADE,
    name text NOT NULL
);

CREATE TABLE demo_events (
    id integer PRIMARY KEY,
    detail text NOT NULL
);

INSERT INTO demo_accounts VALUES
    (1, 'Acme'),
    (2, 'Other');

INSERT INTO demo_projects VALUES
    (10, 1, 'Acme API'),
    (11, 1, 'Acme Web');

INSERT INTO demo_events VALUES
    (1, 'one'),
    (2, 'two'),
    (3, 'three'),
    (4, 'four');
