CREATE TABLE merchants (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name           TEXT        NOT NULL,
    api_key_hash   TEXT        NOT NULL UNIQUE,
    webhook_url    TEXT,
    webhook_secret TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
