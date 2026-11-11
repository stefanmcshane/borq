CREATE TABLE borq_probe_state (
    id BIGINT PRIMARY KEY CHECK (id = 1),
    version BIGINT NOT NULL,
    dataset TEXT NOT NULL,
    checkpoint BIGINT NOT NULL CHECK (checkpoint > 0)
);
CREATE TABLE borq_probe_records (
    id BIGINT PRIMARY KEY,
    label TEXT NOT NULL,
    amount BIGINT NOT NULL,
    optional_text TEXT,
    payload {{binary}} NOT NULL
);
