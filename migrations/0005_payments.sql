-- Phase 08: local mock payment attempts, confirmation events and auditable
-- status transitions. No provider credentials, live requests, phone prompts,
-- PIN data or real financial transactions are stored. Mock attempts are always
-- labelled provider_name='mock' and is_mock=true. Amounts are whole RWF.

-- The existing order statuses remain valid for the current storefront contract.
-- Final payment states are appended; fulfillment never advances automatically.
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_payment_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_payment_status_check
  CHECK (payment_status IN ('pending_payment', 'payment_pending', 'paid', 'payment_failed', 'payment_cancelled'));
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_fulfillment_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_fulfillment_status_check
  CHECK (fulfillment_status IN ('pending', 'payment_received_stock_unavailable'));

-- A consumed reservation is no longer available stock; late successes keep the
-- expired reservation and are recorded as a stock conflict, never as fulfilled.
ALTER TABLE inventory_reservations DROP CONSTRAINT IF EXISTS inventory_reservations_status_check;
ALTER TABLE inventory_reservations ADD CONSTRAINT inventory_reservations_status_check
  CHECK (status IN ('active', 'released', 'expired', 'consumed'));

CREATE TABLE IF NOT EXISTS payment_attempts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id UUID NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    customer_id UUID NOT NULL REFERENCES customers(id) ON DELETE RESTRICT,
    order_reference TEXT NOT NULL CHECK (order_reference ~ '^AV-[0-9]{8}-[A-Z0-9]{12}$'),
    provider_name TEXT NOT NULL CHECK (provider_name IN ('mock', 'xendlypay')),
    provider_environment TEXT NOT NULL CHECK (char_length(btrim(provider_environment)) BETWEEN 1 AND 32),
    is_mock BOOLEAN NOT NULL CHECK (is_mock = (provider_name = 'mock')),
    idempotency_key TEXT NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9._-]{8,128}$'),
    provider_idempotency_key TEXT NOT NULL CHECK (char_length(provider_idempotency_key) BETWEEN 8 AND 128),
    request_fingerprint BYTEA NOT NULL CHECK (octet_length(request_fingerprint) = 32),
    provider_reference TEXT CHECK (provider_reference IS NULL OR (char_length(btrim(provider_reference)) BETWEEN 4 AND 128)),
    amount_rwf BIGINT NOT NULL CHECK (amount_rwf BETWEEN 1 AND 9007199254740991),
    currency TEXT NOT NULL CHECK (currency = 'RWF'),
    customer_phone_e164 TEXT NOT NULL CHECK (customer_phone_e164 ~ '^\+2507[89][0-9]{7}$' OR customer_phone_e164 ~ '^\+2507[23][0-9]{7}$'),
    customer_msisdn TEXT NOT NULL CHECK (customer_msisdn ~ '^2507[89][0-9]{7}$' OR customer_msisdn ~ '^2507[23][0-9]{7}$'),
    customer_phone_display TEXT NOT NULL CHECK (customer_phone_display ~ '^\+250 7[89][0-9] [0-9]{3} [0-9]{3}$' OR customer_phone_display ~ '^\+250 7[23][0-9] [0-9]{3} [0-9]{3}$'),
    network TEXT NOT NULL CHECK (network IN ('mtn', 'airtel')),
    payment_method TEXT NOT NULL CHECK (payment_method = 'prompt'),
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'succeeded', 'failed', 'cancelled')),
    prompt_status TEXT NOT NULL CHECK (prompt_status IN ('not_applicable', 'simulated_pending', 'simulated_approved', 'simulated_rejected', 'simulated_timeout')),
    failure_code TEXT CHECK (failure_code IS NULL OR failure_code IN ('SIMULATED_REJECTED', 'SIMULATED_TIMEOUT', 'STOCK_UNAVAILABLE')),
    failure_message TEXT CHECK (failure_message IS NULL OR char_length(failure_message) <= 240),
    stock_outcome TEXT NOT NULL DEFAULT 'not_applied' CHECK (stock_outcome IN ('not_applied', 'reservation_consumed', 'late_success_blocked', 'reservation_released')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    CHECK (
        (network = 'mtn' AND customer_msisdn ~ '^2507[89][0-9]{7}$' AND customer_phone_e164 ~ '^\+2507[89][0-9]{7}$' AND customer_phone_display ~ '^\+250 7[89][0-9] [0-9]{3} [0-9]{3}$')
        OR (network = 'airtel' AND customer_msisdn ~ '^2507[23][0-9]{7}$' AND customer_phone_e164 ~ '^\+2507[23][0-9]{7}$' AND customer_phone_display ~ '^\+250 7[23][0-9] [0-9]{3} [0-9]{3}$')
    ),
    CHECK ((state IN ('succeeded', 'failed', 'cancelled')) = (completed_at IS NOT NULL)),
    CHECK (state <> 'failed' OR failure_code IS NOT NULL)
);
CREATE UNIQUE INDEX IF NOT EXISTS payment_attempts_order_idempotency_key ON payment_attempts(order_id, idempotency_key);
CREATE UNIQUE INDEX IF NOT EXISTS payment_attempts_provider_key ON payment_attempts(provider_name, provider_environment, provider_idempotency_key);
CREATE UNIQUE INDEX IF NOT EXISTS payment_attempts_provider_reference ON payment_attempts(provider_name, provider_reference) WHERE provider_reference IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS payment_attempts_order_pending ON payment_attempts(order_id) WHERE state = 'pending';
CREATE INDEX IF NOT EXISTS payment_attempts_order_created_idx ON payment_attempts(order_id, created_at DESC, id);
CREATE INDEX IF NOT EXISTS payment_attempts_customer_idx ON payment_attempts(customer_id, created_at DESC);
CREATE INDEX IF NOT EXISTS payment_attempts_state_created_idx ON payment_attempts(state, created_at DESC, id);
CREATE INDEX IF NOT EXISTS payment_attempts_provider_env_idx ON payment_attempts(provider_name, provider_environment, state);

-- The authoritative amount is the order's frozen total. It is copied for fast
-- reporting but cannot be supplied by a client or silently changed later.
CREATE OR REPLACE FUNCTION payment_attempts_authoritative_amount() RETURNS trigger AS $$
DECLARE
    order_total BIGINT;
    order_ref TEXT;
    order_customer UUID;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT o.total_rwf, o.reference, o.customer_id INTO order_total, order_ref, order_customer
        FROM orders o WHERE o.id = NEW.order_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'payment attempt references a missing order' USING ERRCODE = '23503';
        END IF;
        IF NEW.amount_rwf IS DISTINCT FROM order_total OR NEW.order_reference IS DISTINCT FROM order_ref
           OR NEW.customer_id IS DISTINCT FROM order_customer THEN
            RAISE EXCEPTION 'payment amount, reference and customer must match the authoritative order' USING ERRCODE = '23514';
        END IF;
    ELSE
        IF NEW.order_id IS DISTINCT FROM OLD.order_id OR NEW.amount_rwf IS DISTINCT FROM OLD.amount_rwf
           OR NEW.currency IS DISTINCT FROM OLD.currency OR NEW.customer_id IS DISTINCT FROM OLD.customer_id
           OR NEW.order_reference IS DISTINCT FROM OLD.order_reference
           OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
           OR NEW.provider_idempotency_key IS DISTINCT FROM OLD.provider_idempotency_key
           OR NEW.provider_name IS DISTINCT FROM OLD.provider_name
           OR NEW.provider_environment IS DISTINCT FROM OLD.provider_environment
           OR NEW.provider_reference IS DISTINCT FROM OLD.provider_reference
           OR NEW.customer_msisdn IS DISTINCT FROM OLD.customer_msisdn
           OR NEW.customer_phone_display IS DISTINCT FROM OLD.customer_phone_display
           OR NEW.request_fingerprint IS DISTINCT FROM OLD.request_fingerprint THEN
            RAISE EXCEPTION 'payment attempt identity and amount are immutable' USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS payment_attempts_authoritative_amount_trg ON payment_attempts;
CREATE TRIGGER payment_attempts_authoritative_amount_trg
BEFORE INSERT OR UPDATE ON payment_attempts
FOR EACH ROW EXECUTE FUNCTION payment_attempts_authoritative_amount();

CREATE OR REPLACE FUNCTION payment_attempts_touch_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS payment_attempts_touch_updated_at_trg ON payment_attempts;
CREATE TRIGGER payment_attempts_touch_updated_at_trg
BEFORE UPDATE ON payment_attempts
FOR EACH ROW EXECUTE FUNCTION payment_attempts_touch_updated_at();

CREATE TABLE IF NOT EXISTS payment_transitions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    attempt_id UUID NOT NULL REFERENCES payment_attempts(id) ON DELETE RESTRICT,
    from_state TEXT CHECK (from_state IS NULL OR from_state IN ('pending', 'succeeded', 'failed', 'cancelled')),
    to_state TEXT NOT NULL CHECK (to_state IN ('pending', 'succeeded', 'failed', 'cancelled')),
    event_type TEXT NOT NULL CHECK (event_type IN (
        'payment.initiated', 'simulation.approved', 'simulation.rejected',
        'simulation.timeout', 'provider.webhook', 'provider.reconciliation')),
    actor TEXT NOT NULL CHECK (actor IN ('customer', 'mock_simulation', 'provider', 'system')),
    delivery_id TEXT CHECK (delivery_id IS NULL OR (char_length(btrim(delivery_id)) BETWEEN 4 AND 128)),
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS payment_transitions_delivery_key ON payment_transitions(delivery_id) WHERE delivery_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS payment_transitions_attempt_idx ON payment_transitions(attempt_id, occurred_at, id);

-- Future signed webhook deliveries are deduplicated here. No public live
-- webhook endpoint is enabled in this phase; mock events are marked
-- not_applicable because there is no provider signature to verify.
CREATE TABLE IF NOT EXISTS payment_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    attempt_id UUID NOT NULL REFERENCES payment_attempts(id) ON DELETE RESTRICT,
    delivery_id TEXT NOT NULL UNIQUE CHECK (char_length(btrim(delivery_id)) BETWEEN 4 AND 128),
    event_type TEXT NOT NULL CHECK (char_length(btrim(event_type)) BETWEEN 1 AND 80),
    provider_name TEXT NOT NULL CHECK (provider_name IN ('mock', 'xendlypay')),
    provider_environment TEXT NOT NULL CHECK (char_length(btrim(provider_environment)) BETWEEN 1 AND 32),
    verification_status TEXT NOT NULL CHECK (verification_status IN ('not_applicable', 'verified', 'invalid')),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS payment_events_attempt_idx ON payment_events(attempt_id, received_at DESC);

CREATE TABLE IF NOT EXISTS payment_audit_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    attempt_id UUID REFERENCES payment_attempts(id) ON DELETE RESTRICT,
    order_id UUID NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    customer_id UUID NOT NULL REFERENCES customers(id) ON DELETE RESTRICT,
    action TEXT NOT NULL CHECK (char_length(btrim(action)) BETWEEN 1 AND 120),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX IF NOT EXISTS payment_audit_events_attempt_idx ON payment_audit_events(attempt_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS payment_audit_events_order_idx ON payment_audit_events(order_id, occurred_at DESC);
