-- AutivaShop Backend Phase 02: admin authentication, sessions, and audit.
-- Forward-only. No customer, product, order, payment, or WhatsApp tables.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS admin_users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    display_name TEXT NOT NULL,
    role TEXT NOT NULL,
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ NULL,
    CONSTRAINT admin_users_role_check CHECK (role IN ('super_admin', 'admin')),
    CONSTRAINT admin_users_email_format_check CHECK (position('@' in email) > 1)
);

-- Email is stored normalized (lowercase, trimmed). Uniqueness is enforced case-insensitively.
CREATE UNIQUE INDEX IF NOT EXISTS admin_users_email_unique_idx ON admin_users (lower(email));
CREATE INDEX IF NOT EXISTS admin_users_active_idx ON admin_users (is_active);

CREATE TABLE IF NOT EXISTS admin_sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    admin_user_id UUID NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
    token_hash BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ NULL,
    last_seen_at TIMESTAMPTZ NULL,
    created_ip TEXT NULL,
    user_agent TEXT NULL,
    CONSTRAINT admin_sessions_expiry_check CHECK (expires_at > created_at)
);

CREATE UNIQUE INDEX IF NOT EXISTS admin_sessions_token_hash_unique_idx ON admin_sessions (token_hash);
CREATE INDEX IF NOT EXISTS admin_sessions_admin_user_id_idx ON admin_sessions (admin_user_id);
CREATE INDEX IF NOT EXISTS admin_sessions_expires_at_idx ON admin_sessions (expires_at);
CREATE INDEX IF NOT EXISTS admin_sessions_revoked_at_idx ON admin_sessions (revoked_at);

CREATE TABLE IF NOT EXISTS admin_audit_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    admin_user_id UUID NULL REFERENCES admin_users(id) ON DELETE SET NULL,
    action TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    CONSTRAINT admin_audit_events_action_check CHECK (char_length(action) > 0)
);

CREATE INDEX IF NOT EXISTS admin_audit_events_admin_user_id_idx ON admin_audit_events (admin_user_id);
CREATE INDEX IF NOT EXISTS admin_audit_events_action_idx ON admin_audit_events (action);
CREATE INDEX IF NOT EXISTS admin_audit_events_occurred_at_idx ON admin_audit_events (occurred_at DESC);