-- Phase 12: Admin-controlled, audited fulfillment with inventory-safe local
-- delivery in Rwanda. No GPS coordinates, courier API, signature, photo,
-- invented fee, invented location or customer mutation is introduced here.

-- The legacy order column remains the customer-facing status projection. New
-- fulfillment states are appended so existing readers keep working.
ALTER TABLE orders DROP CONSTRAINT IF EXISTS orders_fulfillment_status_check;
ALTER TABLE orders ADD CONSTRAINT orders_fulfillment_status_check
  CHECK (fulfillment_status IN (
    'pending', 'payment_received_stock_unavailable',
    'processing', 'ready_for_delivery', 'out_for_delivery', 'delivered'
  ));

-- One authoritative fulfillment aggregate per order. Delivery metadata is a
-- frozen copy of the existing Rwanda order snapshot; no new location is guessed.
CREATE TABLE order_fulfillment (
    order_id UUID PRIMARY KEY REFERENCES orders(id) ON DELETE RESTRICT,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN (
        'pending', 'processing', 'ready_for_delivery', 'out_for_delivery', 'delivered'
    )),
    version BIGINT NOT NULL DEFAULT 0 CHECK (version >= 0),
    settlement_mode TEXT NOT NULL CHECK (settlement_mode IN (
        'not_paid', 'simulated_dev_test', 'live_verified'
    )),
    delivery_country TEXT NOT NULL DEFAULT 'RW' CHECK (delivery_country = 'RW'),
    delivery_province TEXT NOT NULL CHECK (char_length(btrim(delivery_province)) BETWEEN 1 AND 120),
    delivery_district TEXT NOT NULL CHECK (char_length(btrim(delivery_district)) BETWEEN 1 AND 120),
    delivery_sector TEXT NOT NULL CHECK (char_length(btrim(delivery_sector)) BETWEEN 1 AND 120),
    delivery_cell TEXT CHECK (delivery_cell IS NULL OR char_length(btrim(delivery_cell)) BETWEEN 1 AND 120),
    delivery_address_line TEXT NOT NULL CHECK (char_length(btrim(delivery_address_line)) BETWEEN 1 AND 160),
    delivery_instructions TEXT CHECK (delivery_instructions IS NULL OR char_length(delivery_instructions) <= 500),
    contact_name TEXT NOT NULL CHECK (char_length(btrim(contact_name)) BETWEEN 2 AND 120),
    -- Phone numbers are masked at rest in this projection; raw E.164 remains
    -- only in the protected order/contact surfaces.
    contact_phone_masked TEXT NOT NULL CHECK (contact_phone_masked ~ '^\+2507[0-9]••••[0-9]{3}$'),
    delivered_at TIMESTAMPTZ,
    confirmation_method TEXT CHECK (confirmation_method IS NULL OR confirmation_method = 'admin_manual'),
    confirmation_source TEXT CHECK (confirmation_source IS NULL OR confirmation_source = 'admin_orders_api'),
    confirmation_note TEXT CHECK (confirmation_note IS NULL OR char_length(btrim(confirmation_note)) BETWEEN 1 AND 500),
    confirmed_by_admin_id UUID REFERENCES admin_users(id) ON DELETE SET NULL,
    confirmed_by_session_id UUID REFERENCES admin_sessions(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((status = 'delivered') = (delivered_at IS NOT NULL)),
    CHECK ((status = 'delivered') = (confirmation_method IS NOT NULL)),
    CHECK ((status = 'delivered') = (confirmation_source IS NOT NULL)),
    CHECK ((status = 'delivered') = (confirmed_by_admin_id IS NOT NULL)),
    CHECK ((status = 'delivered') = (confirmed_by_session_id IS NOT NULL))
);
CREATE INDEX order_fulfillment_status_idx ON order_fulfillment(status, updated_at DESC, order_id);
CREATE INDEX order_fulfillment_version_idx ON order_fulfillment(version, order_id);

-- Every newly created order gets a frozen local-delivery fulfillment row.
CREATE OR REPLACE FUNCTION orders_bootstrap_fulfillment() RETURNS trigger AS $$
BEGIN
    INSERT INTO order_fulfillment (
        order_id, status, version, settlement_mode,
        delivery_country, delivery_province, delivery_district, delivery_sector,
        delivery_cell, delivery_address_line, delivery_instructions,
        contact_name, contact_phone_masked
    ) VALUES (
        NEW.id, 'pending', 0, 'not_paid',
        NEW.country, NEW.province, NEW.district, NEW.sector,
        NEW.cell, NEW.address_line, NEW.instructions,
        NEW.customer_name,
        substring(NEW.customer_phone_e164 from 1 for 6) || '••••' || right(NEW.customer_phone_e164, 3)
    );
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS orders_bootstrap_fulfillment_trg ON orders;
CREATE TRIGGER orders_bootstrap_fulfillment_trg
AFTER INSERT ON orders
FOR EACH ROW EXECUTE FUNCTION orders_bootstrap_fulfillment();

-- Append-only transition log. Every successful state change must have a row
-- here with the exact previous state and the incremented version.
CREATE TABLE fulfillment_transitions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id UUID NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    from_status TEXT NOT NULL CHECK (from_status IN (
        'pending', 'processing', 'ready_for_delivery', 'out_for_delivery'
    )),
    to_status TEXT NOT NULL CHECK (to_status IN (
        'processing', 'ready_for_delivery', 'out_for_delivery', 'delivered'
    )),
    version BIGINT NOT NULL CHECK (version > 0),
    event_type TEXT NOT NULL CHECK (event_type IN (
        'fulfillment.processing.started',
        'fulfillment.ready_for_delivery.marked',
        'fulfillment.out_for_delivery.dispatched',
        'fulfillment.delivered.confirmed'
    )),
    actor_type TEXT NOT NULL CHECK (actor_type IN ('admin', 'system')),
    actor_admin_id UUID REFERENCES admin_users(id) ON DELETE SET NULL,
    actor_session_id UUID REFERENCES admin_sessions(id) ON DELETE SET NULL,
    actor_email TEXT CHECK (actor_email IS NULL OR char_length(btrim(actor_email)) BETWEEN 3 AND 320),
    actor_role TEXT CHECK (actor_role IS NULL OR actor_role IN ('admin', 'super_admin')),
    note TEXT CHECK (note IS NULL OR char_length(btrim(note)) BETWEEN 1 AND 500),
    idempotency_key TEXT NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9._-]{8,128}$'),
    request_fingerprint BYTEA NOT NULL CHECK (octet_length(request_fingerprint) = 32),
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((from_status, to_status, event_type) IN (
        ('pending', 'processing', 'fulfillment.processing.started'),
        ('processing', 'ready_for_delivery', 'fulfillment.ready_for_delivery.marked'),
        ('ready_for_delivery', 'out_for_delivery', 'fulfillment.out_for_delivery.dispatched'),
        ('out_for_delivery', 'delivered', 'fulfillment.delivered.confirmed')
    )),
    CHECK ((actor_type = 'system' AND actor_admin_id IS NULL AND actor_session_id IS NULL
            AND actor_email IS NULL AND actor_role IS NULL)
        OR (actor_type = 'admin' AND actor_admin_id IS NOT NULL AND actor_session_id IS NOT NULL
            AND char_length(btrim(COALESCE(actor_email, ''))) BETWEEN 3 AND 320
            AND actor_role IN ('admin', 'super_admin')))
);
CREATE UNIQUE INDEX fulfillment_transitions_order_version ON fulfillment_transitions(order_id, version);
CREATE UNIQUE INDEX fulfillment_transitions_idempotency ON fulfillment_transitions(order_id, idempotency_key);
CREATE INDEX fulfillment_transitions_order_idx ON fulfillment_transitions(order_id, occurred_at DESC, id);
CREATE INDEX fulfillment_transitions_actor_idx ON fulfillment_transitions(actor_admin_id, occurred_at DESC);

-- Explicit per-product consumption record. Successful mock settlement in the
-- payment migration consumes the reservation once; this table records that
-- fact without ever adjusting stock a second time.
CREATE TABLE inventory_consumption_events (
    order_id UUID NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    product_id UUID NOT NULL REFERENCES products(id) ON DELETE RESTRICT,
    reservation_id UUID NOT NULL UNIQUE REFERENCES inventory_reservations(id) ON DELETE RESTRICT,
    quantity BIGINT NOT NULL CHECK (quantity BETWEEN 1 AND 9007199254740991),
    source TEXT NOT NULL CHECK (source IN ('payment_simulation', 'backfill_legacy')),
    consumed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (order_id, product_id)
);
CREATE INDEX inventory_consumption_events_order_idx ON inventory_consumption_events(order_id);

-- Backfill existing orders. A legacy stock conflict is kept on orders.fulfillment_status
-- and is surfaced as a fulfillment conflict; it is never silently repaired.
INSERT INTO order_fulfillment (
    order_id, status, version, settlement_mode,
    delivery_country, delivery_province, delivery_district, delivery_sector,
    delivery_cell, delivery_address_line, delivery_instructions,
    contact_name, contact_phone_masked
)
SELECT o.id, 'pending', 0,
    CASE
        WHEN EXISTS (SELECT 1 FROM payment_attempts pa
                     WHERE pa.order_id=o.id AND pa.state='succeeded' AND pa.is_mock) THEN 'simulated_dev_test'
        WHEN EXISTS (SELECT 1 FROM payment_attempts pa
                     WHERE pa.order_id=o.id AND pa.state='succeeded' AND NOT pa.is_mock) THEN 'live_verified'
        ELSE 'not_paid'
    END,
    o.country, o.province, o.district, o.sector, o.cell, o.address_line, o.instructions,
    o.customer_name,
    substring(o.customer_phone_e164 from 1 for 6) || '••••' || right(o.customer_phone_e164, 3)
FROM orders o
ON CONFLICT (order_id) DO NOTHING;

INSERT INTO inventory_consumption_events (order_id, product_id, reservation_id, quantity, source, consumed_at)
SELECT r.order_id, r.product_id, r.id, r.quantity, 'backfill_legacy', r.created_at
FROM inventory_reservations r
WHERE r.status = 'consumed'
ON CONFLICT (order_id, product_id) DO NOTHING;

-- Touch timestamps without allowing clients to supply them.
CREATE OR REPLACE FUNCTION fulfillment_touch_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS order_fulfillment_touch_updated_at_trg ON order_fulfillment;
CREATE TRIGGER order_fulfillment_touch_updated_at_trg
BEFORE INSERT OR UPDATE ON order_fulfillment
FOR EACH ROW EXECUTE FUNCTION fulfillment_touch_updated_at();

-- Validate a transition row against the locked aggregate before it is written.
CREATE OR REPLACE FUNCTION fulfillment_transitions_validate() RETURNS trigger AS $$
DECLARE
    current_status TEXT;
    current_version BIGINT;
BEGIN
    SELECT status, version INTO current_status, current_version
    FROM order_fulfillment WHERE order_id = NEW.order_id FOR UPDATE;
    IF NOT FOUND OR current_status IS DISTINCT FROM NEW.from_status
       OR current_version + 1 <> NEW.version THEN
        RAISE EXCEPTION 'fulfillment transition does not follow the locked current state'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS fulfillment_transitions_validate_trg ON fulfillment_transitions;
CREATE TRIGGER fulfillment_transitions_validate_trg
BEFORE INSERT ON fulfillment_transitions
FOR EACH ROW EXECUTE FUNCTION fulfillment_transitions_validate();

-- Audit rows are immutable. Controlled cleanup for isolated tests requires an
-- explicit session flag; ordinary requests can never alter or erase history.
CREATE OR REPLACE FUNCTION fulfillment_audit_immutable() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'fulfillment audit events are immutable' USING ERRCODE = '23514';
    END IF;
    IF COALESCE(current_setting('autivashop.fulfillment_cleanup', true), '') <> 'on' THEN
        RAISE EXCEPTION 'fulfillment audit events are immutable' USING ERRCODE = '23514';
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS fulfillment_transitions_immutable_trg ON fulfillment_transitions;
CREATE TRIGGER fulfillment_transitions_immutable_trg
BEFORE UPDATE OR DELETE ON fulfillment_transitions
FOR EACH ROW EXECUTE FUNCTION fulfillment_audit_immutable();

DROP TRIGGER IF EXISTS inventory_consumption_events_immutable_trg ON inventory_consumption_events;
CREATE TRIGGER inventory_consumption_events_immutable_trg
BEFORE UPDATE OR DELETE ON inventory_consumption_events
FOR EACH ROW EXECUTE FUNCTION fulfillment_audit_immutable();

-- The aggregate itself may only advance via the matching audited transition.
-- Delivery/confirmation metadata is immutable after it is first recorded.
CREATE OR REPLACE FUNCTION order_fulfillment_guard() RETURNS trigger AS $$
DECLARE
    has_audit BOOLEAN;
BEGIN
    IF OLD.status = 'delivered' THEN
        RAISE EXCEPTION 'delivered fulfillment cannot be changed' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <> OLD.version + 1 OR NEW.status = OLD.status THEN
        RAISE EXCEPTION 'fulfillment version or status is invalid' USING ERRCODE = '23514';
    END IF;
    IF NEW.settlement_mode IS DISTINCT FROM OLD.settlement_mode
       AND NOT (OLD.settlement_mode = 'not_paid'
                AND NEW.settlement_mode IN ('simulated_dev_test', 'live_verified')) THEN
        RAISE EXCEPTION 'settlement mode cannot be changed' USING ERRCODE = '23514';
    END IF;
    IF NEW.delivery_country IS DISTINCT FROM OLD.delivery_country
       OR NEW.delivery_province IS DISTINCT FROM OLD.delivery_province
       OR NEW.delivery_district IS DISTINCT FROM OLD.delivery_district
       OR NEW.delivery_sector IS DISTINCT FROM OLD.delivery_sector
       OR NEW.delivery_cell IS DISTINCT FROM OLD.delivery_cell
       OR NEW.delivery_address_line IS DISTINCT FROM OLD.delivery_address_line
       OR NEW.delivery_instructions IS DISTINCT FROM OLD.delivery_instructions
       OR NEW.contact_name IS DISTINCT FROM OLD.contact_name
       OR NEW.contact_phone_masked IS DISTINCT FROM OLD.contact_phone_masked THEN
        RAISE EXCEPTION 'local delivery snapshot is immutable' USING ERRCODE = '23514';
    END IF;
    SELECT EXISTS(
        SELECT 1 FROM fulfillment_transitions t
        WHERE t.order_id = NEW.order_id AND t.version = NEW.version
          AND t.from_status = OLD.status AND t.to_status = NEW.status
    ) INTO has_audit;
    IF NOT has_audit THEN
        RAISE EXCEPTION 'fulfillment state change requires a matching audit transition'
            USING ERRCODE = '23514';
    END IF;
    IF NEW.status = 'delivered' THEN
        IF NEW.delivered_at IS NULL OR NEW.confirmed_by_admin_id IS NULL
           OR NEW.confirmed_by_session_id IS NULL OR NEW.confirmation_method IS NULL
           OR NEW.confirmation_source IS NULL THEN
            RAISE EXCEPTION 'delivery confirmation metadata is incomplete' USING ERRCODE = '23514';
        END IF;
    ELSIF NEW.delivered_at IS NOT NULL OR NEW.confirmed_by_admin_id IS NOT NULL
          OR NEW.confirmed_by_session_id IS NOT NULL OR NEW.confirmation_method IS NOT NULL
          OR NEW.confirmation_source IS NOT NULL THEN
        RAISE EXCEPTION 'delivery confirmation is only valid in the delivered state'
            USING ERRCODE = '23514';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS order_fulfillment_guard_trg ON order_fulfillment;
CREATE TRIGGER order_fulfillment_guard_trg
BEFORE UPDATE ON order_fulfillment
FOR EACH ROW EXECUTE FUNCTION order_fulfillment_guard();
