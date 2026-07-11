-- 0001_identity.pg.sql — identity-auth production schema (PostgreSQL).
-- Mirrors the S-T3 pattern (libs/idempotency ships a .pg.sql + a SQLite twin in
-- code): PG is the durable engine in prod; tests run the SQLite equivalent in
-- store.go so the concurrency/rotation criteria need no server.
--
-- D4: identity holds the credential + session of record; the ACCESS token is
-- stateless (verified at the edge) so this schema is off the request hot path.

CREATE TABLE IF NOT EXISTS users (
    user_id     TEXT        NOT NULL PRIMARY KEY,      -- usr_<ulid>
    email       TEXT        NOT NULL,
    pw_hash     TEXT        NOT NULL,                  -- pbkdf2-sha256$iter$salt$dk
    role        TEXT        NOT NULL DEFAULT 'customer',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Email is the login identity; unique, case-folded at the app layer.
CREATE UNIQUE INDEX IF NOT EXISTS users_email_uidx ON users (email);

CREATE TABLE IF NOT EXISTS sessions (
    session_id    TEXT        NOT NULL PRIMARY KEY,    -- ses_<ulid> (refresh family)
    user_id       TEXT        NOT NULL REFERENCES users (user_id),
    refresh_hash  TEXT        NOT NULL,                -- sha256(opaque refresh token)
    access_jti    TEXT        NOT NULL,                -- jti of the most-recent access token
    role          TEXT        NOT NULL DEFAULT 'customer',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL,
    revoked       BOOLEAN     NOT NULL DEFAULT false
);
-- Rotation replaces refresh_hash in place; a leaked-but-rotated token no longer
-- matches. Unique so a stolen hash can't be double-spent across sessions.
CREATE UNIQUE INDEX IF NOT EXISTS sessions_refresh_uidx ON sessions (refresh_hash);
CREATE INDEX IF NOT EXISTS sessions_user_idx ON sessions (user_id);
