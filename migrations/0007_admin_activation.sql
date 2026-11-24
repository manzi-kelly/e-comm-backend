-- AutivaShop: one-time, email-verified activation of the initial super_admin.
-- Forward-only. This migration provisions no administrator and sends no email.
-- Only explicit development/test setup commands may insert an authorization.

CREATE TABLE IF NOT EXISTS admin_activation_authorizations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email TEXT NOT NULL,
    display_name TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'super_admin',
    status TEXT NOT NULL DEFAULT 'pending',
    activation_token_hash BYTEA NULL,
    activation_expires_at TIMESTAMPTZ NULL,
    activated_user_id UUID NULL REFERENCES admin_users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    activated_at TIMESTAMPTZ NULL,
    CONSTRAINT admin_activation_authorizations_email_check
        CHECK (email = lower(email) AND position('@' in email) > 1 AND char_length(email) <= 320),
    CONSTRAINT admin_activation_authorizations_role_check CHECK (role = 'super_admin'),
    CONSTRAINT admin_activation_authorizations_status_check CHECK (status IN ('pending', 'activated')),
    CONSTRAINT admin_activation_authorizations_display_name_check
        CHECK (char_length(btrim(display_name)) BETWEEN 1 AND 120),
    CONSTRAINT admin_activation_authorizations_failed_check
        CHECK (activated_at IS NULL OR (status = 'activated' AND activated_user_id IS NOT NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS admin_activation_authorizations_email_unique_idx
    ON admin_activation_authorizations (lower(email));
-- At most one pending authorization and at most one completed activation can
-- ever exist. These partial unique indexes make one-time activation a database
-- invariant, not just an application convention.
CREATE UNIQUE INDEX IF NOT EXISTS admin_activation_authorizations_single_pending_idx
    ON admin_activation_authorizations ((status = 'pending')) WHERE status = 'pending';
CREATE UNIQUE INDEX IF NOT EXISTS admin_activation_authorizations_single_activated_idx
    ON admin_activation_authorizations ((status = 'activated')) WHERE status = 'activated';

CREATE TABLE IF NOT EXISTS admin_activation_codes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    authorization_id UUID NOT NULL
        REFERENCES admin_activation_authorizations(id) ON DELETE CASCADE,
    -- Keyed HMAC-SHA256 over the authorized email and the six-digit code.
    -- Neither the code nor any reversible form of it is stored.
    code_hash BYTEA NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    failed_attempts INTEGER NOT NULL DEFAULT 0,
    consumed_at TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_ip TEXT NULL,
    CONSTRAINT admin_activation_codes_attempts_check CHECK (failed_attempts >= 0),
    CONSTRAINT admin_activation_codes_hash_check CHECK (octet_length(code_hash) = 32)
);

CREATE INDEX IF NOT EXISTS admin_activation_codes_authorization_idx
    ON admin_activation_codes (authorization_id, consumed_at, created_at DESC);
CREATE INDEX IF NOT EXISTS admin_activation_codes_created_at_idx
    ON admin_activation_codes (created_at);
