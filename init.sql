CREATE TABLE IF NOT EXISTS urls (
  code        TEXT PRIMARY KEY,
  target_url  TEXT NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS clicks (
  id          BIGSERIAL PRIMARY KEY,
  code        TEXT NOT NULL REFERENCES urls(code) ON DELETE CASCADE,
  ts          TIMESTAMPTZ NOT NULL DEFAULT now(),
  ip          INET,
  user_agent  TEXT,
  device      TEXT,
  os          TEXT,
  browser     TEXT
);
CREATE INDEX IF NOT EXISTS idx_clicks_code_ts ON clicks(code, ts DESC);
