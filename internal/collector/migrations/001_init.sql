CREATE TABLE IF NOT EXISTS batches (
    agent_id uuid NOT NULL,
    batch_id text NOT NULL,
    site_id text NOT NULL,
    agent_version text NOT NULL,
    sent_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    event_count integer NOT NULL,
    PRIMARY KEY (agent_id, batch_id)
);

CREATE TABLE IF NOT EXISTS events (
    id bigserial PRIMARY KEY,
    agent_id uuid NOT NULL,
    batch_id text NOT NULL,
    site_id text NOT NULL,
    event_key text NOT NULL,
    event_id text,
    event_received_at timestamptz NOT NULL,
    source_ip inet,
    raw text NOT NULL,
    raw_base64 text,
    raw_hash text NOT NULL,
    parsed jsonb,
    parse_error text,
    received_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (agent_id, event_key),
    FOREIGN KEY (agent_id, batch_id) REFERENCES batches(agent_id, batch_id)
);
CREATE INDEX IF NOT EXISTS events_received_at_idx ON events(received_at);
CREATE INDEX IF NOT EXISTS events_site_id_idx ON events(site_id);
CREATE INDEX IF NOT EXISTS events_event_id_idx ON events(event_id) WHERE event_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS heartbeats (
    agent_id uuid PRIMARY KEY,
    site_id text NOT NULL,
    agent_version text NOT NULL,
    sent_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    payload jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS heartbeats_received_at_idx ON heartbeats(received_at);
