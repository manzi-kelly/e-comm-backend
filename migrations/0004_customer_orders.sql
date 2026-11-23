-- Phase 06: customer accounts, private sessions and authoritative orders.
-- No payment collection, payment screenshots, WhatsApp data or seed records.
-- All monetary values are whole RWF. Delivery fees are explicitly unavailable
-- in this phase, so total always equals subtotal.

CREATE TABLE customers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL CHECK (char_length(btrim(name)) BETWEEN 2 AND 120),
    email TEXT NOT NULL CHECK (email = lower(btrim(email)) AND char_length(email) BETWEEN 3 AND 254),
    phone_e164 TEXT NOT NULL CHECK (phone_e164 ~ '^\+2507[0-9]{8}$'),
    phone_display TEXT NOT NULL CHECK (char_length(phone_display) BETWEEN 1 AND 32),
    password_hash TEXT NOT NULL CHECK (password_hash LIKE '$argon2id$%'),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX customers_email_key ON customers(email);
CREATE UNIQUE INDEX customers_phone_key ON customers(phone_e164);
CREATE INDEX customers_status_created_idx ON customers(status, created_at DESC, id);

CREATE TABLE customer_sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id UUID NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    token_hash BYTEA NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    last_seen_at TIMESTAMPTZ,
    created_ip TEXT CHECK (char_length(created_ip) <= 512),
    user_agent TEXT CHECK (char_length(user_agent) <= 512),
    CHECK (expires_at > created_at),
    CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);
CREATE INDEX customer_sessions_customer_idx ON customer_sessions(customer_id, created_at DESC);
CREATE INDEX customer_sessions_expires_idx ON customer_sessions(expires_at);
CREATE INDEX customer_sessions_active_idx ON customer_sessions(customer_id) WHERE revoked_at IS NULL;

CREATE TABLE customer_audit_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id UUID REFERENCES customers(id) ON DELETE SET NULL,
    action TEXT NOT NULL CHECK (char_length(btrim(action)) BETWEEN 1 AND 120),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX customer_audit_events_customer_idx ON customer_audit_events(customer_id, occurred_at DESC);
CREATE INDEX customer_audit_events_action_idx ON customer_audit_events(action, occurred_at DESC);

CREATE TABLE orders (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    reference TEXT NOT NULL UNIQUE CHECK (reference ~ '^AV-[0-9]{8}-[A-Z0-9]{12}$'),
    customer_id UUID REFERENCES customers(id) ON DELETE RESTRICT,
    idempotency_key TEXT CHECK (idempotency_key IS NULL OR (char_length(idempotency_key) BETWEEN 8 AND 128 AND idempotency_key ~ '^[A-Za-z0-9._-]+$')),
    request_fingerprint BYTEA CHECK (request_fingerprint IS NULL OR octet_length(request_fingerprint) = 32),
    customer_name TEXT NOT NULL CHECK (char_length(btrim(customer_name)) BETWEEN 2 AND 120),
    customer_email TEXT NOT NULL CHECK (char_length(btrim(customer_email)) BETWEEN 3 AND 254),
    customer_phone_e164 TEXT NOT NULL CHECK (customer_phone_e164 ~ '^\+2507[0-9]{8}$'),
    customer_phone_display TEXT NOT NULL CHECK (char_length(customer_phone_display) BETWEEN 1 AND 32),
    country TEXT NOT NULL CHECK (country = 'RW'),
    province TEXT NOT NULL CHECK (char_length(btrim(province)) BETWEEN 1 AND 120),
    district TEXT NOT NULL CHECK (char_length(btrim(district)) BETWEEN 1 AND 120),
    sector TEXT NOT NULL CHECK (char_length(btrim(sector)) BETWEEN 1 AND 120),
    cell TEXT CHECK (cell IS NULL OR char_length(btrim(cell)) BETWEEN 1 AND 120),
    address_line TEXT NOT NULL CHECK (char_length(btrim(address_line)) BETWEEN 1 AND 160),
    instructions TEXT CHECK (instructions IS NULL OR char_length(instructions) <= 500),
    subtotal_rwf BIGINT NOT NULL CHECK (subtotal_rwf BETWEEN 1 AND 9007199254740991),
    delivery_fee_rwf BIGINT NOT NULL CHECK (delivery_fee_rwf = 0),
    delivery_fee_status TEXT NOT NULL CHECK (delivery_fee_status = 'unavailable'),
    total_rwf BIGINT NOT NULL CHECK (total_rwf BETWEEN 1 AND 9007199254740991),
    payment_status TEXT NOT NULL DEFAULT 'pending_payment' CHECK (payment_status = 'pending_payment'),
    fulfillment_status TEXT NOT NULL DEFAULT 'pending' CHECK (fulfillment_status = 'pending'),
    created_ip TEXT CHECK (char_length(created_ip) <= 512),
    user_agent TEXT CHECK (char_length(user_agent) <= 512),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (total_rwf = subtotal_rwf + delivery_fee_rwf),
    CHECK ((customer_id IS NULL AND idempotency_key IS NULL AND request_fingerprint IS NULL)
        OR (customer_id IS NOT NULL AND idempotency_key IS NOT NULL AND request_fingerprint IS NOT NULL))
);
CREATE UNIQUE INDEX orders_customer_idempotency_key ON orders(customer_id, idempotency_key) WHERE customer_id IS NOT NULL;
CREATE INDEX orders_customer_created_idx ON orders(customer_id, created_at DESC, id);
CREATE INDEX orders_reference_lookup_idx ON orders(reference);

CREATE TABLE order_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id UUID NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    product_id UUID NOT NULL REFERENCES products(id) ON DELETE RESTRICT,
    snapshot_name_en TEXT NOT NULL CHECK (char_length(snapshot_name_en) BETWEEN 1 AND 240),
    snapshot_name_rw TEXT CHECK (snapshot_name_rw IS NULL OR char_length(snapshot_name_rw) <= 240),
    snapshot_sku TEXT NOT NULL CHECK (snapshot_sku ~ '^[A-Z0-9][A-Z0-9_-]{2,63}$'),
    snapshot_condition TEXT NOT NULL CHECK (snapshot_condition IN ('BRAND_NEW_SEALED','BRAND_NEW_OPEN_BOX','REFURBISHED','USED_GRADE_A','USED_GRADE_B','USED_GRADE_C')),
    quantity INTEGER NOT NULL CHECK (quantity BETWEEN 1 AND 99),
    unit_price_rwf BIGINT NOT NULL CHECK (unit_price_rwf BETWEEN 1 AND 9007199254740991),
    line_total_rwf BIGINT NOT NULL CHECK (line_total_rwf BETWEEN 1 AND 9007199254740991),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (line_total_rwf = unit_price_rwf * quantity),
    UNIQUE (order_id, product_id)
);
CREATE INDEX order_items_order_idx ON order_items(order_id, created_at, id);
CREATE INDEX order_items_product_idx ON order_items(product_id);

-- Available stock is stock_quantity minus active, unexpired reservations.
-- No on-hand quantity is deducted at order creation; expiry releases stock
-- automatically because expired rows stop counting as active.
CREATE TABLE inventory_reservations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    product_id UUID NOT NULL REFERENCES products(id) ON DELETE RESTRICT,
    order_id UUID NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    quantity BIGINT NOT NULL CHECK (quantity BETWEEN 1 AND 9007199254740991),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'released', 'expired')),
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at TIMESTAMPTZ,
    CHECK (expires_at > created_at),
    CHECK (status <> 'released' OR released_at IS NOT NULL)
);
CREATE UNIQUE INDEX inventory_reservations_order_product_key ON inventory_reservations(order_id, product_id);
CREATE INDEX inventory_reservations_product_active_idx ON inventory_reservations(product_id, expires_at) WHERE status = 'active';
CREATE INDEX inventory_reservations_order_idx ON inventory_reservations(order_id);

CREATE TABLE order_audit_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id UUID REFERENCES orders(id) ON DELETE RESTRICT,
    customer_id UUID REFERENCES customers(id) ON DELETE SET NULL,
    action TEXT NOT NULL CHECK (char_length(btrim(action)) BETWEEN 1 AND 120),
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX order_audit_events_order_idx ON order_audit_events(order_id, occurred_at DESC);
CREATE INDEX order_audit_events_customer_idx ON order_audit_events(customer_id, occurred_at DESC);
