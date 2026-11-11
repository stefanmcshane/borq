SET timezone = 'UTC';
BEGIN;
CREATE SCHEMA recovery;
CREATE TABLE recovery.accounts (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name text NOT NULL UNIQUE,
  enabled boolean NOT NULL,
  balance numeric(24,8) NOT NULL CHECK (balance >= 0),
  optional_text text,
  payload bytea NOT NULL,
  created_at timestamptz NOT NULL,
  birthday date NOT NULL,
  token uuid NOT NULL,
  document jsonb NOT NULL,
  tags text[] NOT NULL,
  precise double precision NOT NULL
);
INSERT INTO recovery.accounts (name, enabled, balance, optional_text, payload, created_at, birthday, token, document, tags, precise)
SELECT 'account-' || lpad(n::text, 4, '0') || '-雪-🙂', n % 2 = 0,
       n::numeric * 12345.00000001,
       CASE WHEN n % 3 = 0 THEN NULL WHEN n % 3 = 1 THEN '' ELSE E'line\n雪\tquote\'' END,
       decode(lpad(to_hex(n), 4, '0') || '00ff80', 'hex'),
       timestamptz '2024-01-01 00:00:00+00' + n * interval '1234567 microseconds',
       date '2000-01-01' + n,
       ('00000000-0000-0000-0000-' || lpad(n::text, 12, '0'))::uuid,
       jsonb_build_object('n', n, 'null', NULL, 'unicode', '雪🙂', 'empty', ''),
       ARRAY['', '雪', NULL, n::text], n::double precision / 8
FROM generate_series(1, 257) n;
CREATE TABLE recovery.entries (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  account_id bigint NOT NULL REFERENCES recovery.accounts(id),
  ordinal integer NOT NULL,
  note varchar(80) NOT NULL,
  occurred_at timestamp NOT NULL,
  UNIQUE (account_id, ordinal)
);
CREATE INDEX entries_account_id_idx ON recovery.entries(account_id);
CREATE INDEX accounts_enabled_name_idx ON recovery.accounts(name) WHERE enabled;
INSERT INTO recovery.entries (account_id, ordinal, note, occurred_at)
SELECT n, ordinal, 'entry-雪-' || ordinal, timestamp '2024-06-01 12:34:56.123456' + n * interval '1 second'
FROM generate_series(1, 257) n CROSS JOIN generate_series(1, 3) ordinal;
CREATE TABLE recovery.duplicates (value text, payload bytea);
INSERT INTO recovery.duplicates VALUES
  (NULL, NULL), (NULL, NULL), ('', '\x'::bytea), ('', '\x'::bytea),
  ('雪🙂', '\x00ff80'::bytea), ('雪🙂', '\x00ff80'::bytea);
CREATE TABLE recovery.empty_table (id integer PRIMARY KEY, value text);
CREATE SEQUENCE recovery.ticket_number AS bigint INCREMENT BY 7 MINVALUE 11 START WITH 11 CACHE 1;
SELECT nextval('recovery.ticket_number') FROM generate_series(1, 9);
COMMIT;
