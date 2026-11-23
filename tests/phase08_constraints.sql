-- Isolated PostgreSQL verification for Phase 08. All synthetic rows roll back.
\set ON_ERROR_STOP on
BEGIN;
DO $$
DECLARE
    pid uuid;
    cid uuid;
    oid uuid;
    aid uuid;
    n integer;
BEGIN
    SELECT count(*) INTO n FROM schema_migrations
    WHERE version IN (
        '0001_foundation.sql', '0002_admin_auth.sql', '0003_products.sql',
        '0004_customer_orders.sql', '0005_payments.sql');
    IF n <> 5 THEN RAISE EXCEPTION 'missing Phase 08 migration record'; END IF;

    INSERT INTO products(sku,name_en,description_en,description_rw,category,price_rwf,condition,country,published,published_at)
    VALUES ('PHASE08-SQL-SKU','Phase08 SQL constraint test','Test','Test','audio',1000,'USED_GRADE_A','RW',true,now())
    RETURNING id INTO pid;
    INSERT INTO inventory(product_id,stock_quantity,low_stock_threshold,availability)
    VALUES (pid,3,1,'available');

    INSERT INTO customers(name,email,phone_e164,phone_display,password_hash,status)
    VALUES ('Phase08 Test','phase08-sql@example.test','+250781234567','+250 781 234 567',
            '$argon2id$v=19$m=65536,t=3,p=1$c2FsdA$aGFzaA','active')
    RETURNING id INTO cid;

    INSERT INTO orders(reference,customer_id,idempotency_key,request_fingerprint,customer_name,customer_email,
        customer_phone_e164,customer_phone_display,country,province,district,sector,address_line,
        subtotal_rwf,delivery_fee_rwf,delivery_fee_status,total_rwf)
    VALUES ('AV-20261123-ABCDEFGHJKLM',cid,'phase08-sql-order',decode(repeat('A0',32),'hex'),
        'Phase08 Test','phase08-sql@example.test','+250781234567','+250 781 234 567',
        'RW','Kigali','Gasabo','Remera','KG 123 St',1000,0,'unavailable',1000)
    RETURNING id INTO oid;

    INSERT INTO inventory_reservations(product_id,order_id,quantity,status,expires_at)
    VALUES (pid,oid,1,'active',now()+interval '30 minutes');

    INSERT INTO payment_attempts(order_id,customer_id,order_reference,provider_name,provider_environment,is_mock,
        idempotency_key,provider_idempotency_key,request_fingerprint,provider_reference,amount_rwf,currency,
        customer_phone_e164,customer_msisdn,customer_phone_display,network,payment_method,state,prompt_status,stock_outcome)
    VALUES (oid,cid,'AV-20261123-ABCDEFGHJKLM','mock','test',true,
        'phase08-sql-idem','av-aaaaaaaaaaaaaaaaaaaaaaaa',decode(repeat('B0',32),'hex'),'MOCK-AAAAAAAAAAAAAAAA',
        1000,'RWF','+250781234567','250781234567','+250 781 234 567','mtn','prompt','pending','simulated_pending','not_applied')
    RETURNING id INTO aid;

    BEGIN
        INSERT INTO payment_attempts(order_id,customer_id,order_reference,provider_name,provider_environment,is_mock,
            idempotency_key,provider_idempotency_key,request_fingerprint,provider_reference,amount_rwf,currency,
            customer_phone_e164,customer_msisdn,customer_phone_display,network,payment_method,state,prompt_status,stock_outcome)
        VALUES (oid,cid,'AV-20261123-ABCDEFGHJKLM','mock','test',true,
            'phase08-sql-badamount','av-bbbbbbbbbbbbbbbbbbbbbbbb',decode(repeat('C0',32),'hex'),'MOCK-BBBBBBBBBBBBBBBB',
            999,'RWF','+250781234567','250781234567','+250 781 234 567','mtn','prompt','pending','simulated_pending','not_applied');
        RAISE EXCEPTION 'authoritative amount trigger missing';
    EXCEPTION WHEN check_violation THEN NULL; END;

    BEGIN
        INSERT INTO payment_attempts(order_id,customer_id,order_reference,provider_name,provider_environment,is_mock,
            idempotency_key,provider_idempotency_key,request_fingerprint,provider_reference,amount_rwf,currency,
            customer_phone_e164,customer_msisdn,customer_phone_display,network,payment_method,state,prompt_status,stock_outcome)
        VALUES (oid,cid,'AV-20261123-ABCDEFGHJKLM','mock','test',true,
            'phase08-sql-badnetwork','av-cccccccccccccccccccccccc',decode(repeat('D0',32),'hex'),'MOCK-CCCCCCCCCCCCCCCC',
            1000,'RWF','+250721234567','250721234567','+250 721 234 567','mtn','prompt','pending','simulated_pending','not_applied');
        RAISE EXCEPTION 'phone/network consistency check missing';
    EXCEPTION WHEN check_violation THEN NULL; END;

    BEGIN
        INSERT INTO payment_attempts(order_id,customer_id,order_reference,provider_name,provider_environment,is_mock,
            idempotency_key,provider_idempotency_key,request_fingerprint,provider_reference,amount_rwf,currency,
            customer_phone_e164,customer_msisdn,customer_phone_display,network,payment_method,state,prompt_status,stock_outcome)
        VALUES (oid,cid,'AV-20261123-ABCDEFGHJKLM','mock','test',false,
            'phase08-sql-badmock','av-dddddddddddddddddddddddd',decode(repeat('E0',32),'hex'),'MOCK-DDDDDDDDDDDDDDDD',
            1000,'RWF','+250781234567','250781234567','+250 781 234 567','mtn','prompt','pending','simulated_pending','not_applied');
        RAISE EXCEPTION 'mock/live classification check missing';
    EXCEPTION WHEN check_violation THEN NULL; END;

    BEGIN
        INSERT INTO payment_attempts(order_id,customer_id,order_reference,provider_name,provider_environment,is_mock,
            idempotency_key,provider_idempotency_key,request_fingerprint,provider_reference,amount_rwf,currency,
            customer_phone_e164,customer_msisdn,customer_phone_display,network,payment_method,state,prompt_status,stock_outcome)
        VALUES (oid,cid,'AV-20261123-ABCDEFGHJKLM','paypal','test',false,
            'phase08-sql-badprovider','av-eeeeeeeeeeeeeeeeeeeeeeee',decode(repeat('F0',32),'hex'),'LIVE-EEEEEEEEEEEEEEEE',
            1000,'RWF','+250781234567','250781234567','+250 781 234 567','mtn','prompt','pending','simulated_pending','not_applied');
        RAISE EXCEPTION 'unknown provider check missing';
    EXCEPTION WHEN check_violation THEN NULL; END;

    BEGIN
        INSERT INTO payment_attempts(order_id,customer_id,order_reference,provider_name,provider_environment,is_mock,
            idempotency_key,provider_idempotency_key,request_fingerprint,provider_reference,amount_rwf,currency,
            customer_phone_e164,customer_msisdn,customer_phone_display,network,payment_method,state,prompt_status,stock_outcome)
        VALUES (oid,cid,'AV-20261123-ABCDEFGHJKLM','mock','test',true,
            'phase08-sql-idem','av-ffffffffffffffffffffffff',decode(repeat('11',32),'hex'),'MOCK-FFFFFFFFFFFFFFFF',
            1000,'RWF','+250781234567','250781234567','+250 781 234 567','mtn','prompt','pending','simulated_pending','not_applied');
        RAISE EXCEPTION 'order idempotency uniqueness missing';
    EXCEPTION WHEN unique_violation THEN NULL; END;

    BEGIN
        INSERT INTO payment_attempts(order_id,customer_id,order_reference,provider_name,provider_environment,is_mock,
            idempotency_key,provider_idempotency_key,request_fingerprint,provider_reference,amount_rwf,currency,
            customer_phone_e164,customer_msisdn,customer_phone_display,network,payment_method,state,prompt_status,stock_outcome)
        VALUES (oid,cid,'AV-20261123-ABCDEFGHJKLM','mock','test',true,
            'phase08-sql-dupref','av-222222222222222222222222',decode(repeat('22',32),'hex'),'MOCK-AAAAAAAAAAAAAAAA',
            1000,'RWF','+250781234567','250781234567','+250 781 234 567','mtn','prompt','pending','simulated_pending','not_applied');
        RAISE EXCEPTION 'provider reference uniqueness missing';
    EXCEPTION WHEN unique_violation THEN NULL; END;

    BEGIN
        INSERT INTO payment_attempts(order_id,customer_id,order_reference,provider_name,provider_environment,is_mock,
            idempotency_key,provider_idempotency_key,request_fingerprint,provider_reference,amount_rwf,currency,
            customer_phone_e164,customer_msisdn,customer_phone_display,network,payment_method,state,prompt_status,stock_outcome)
        VALUES (oid,cid,'AV-20261123-ABCDEFGHJKLM','mock','test',true,
            'phase08-sql-pending2','av-333333333333333333333333',decode(repeat('33',32),'hex'),'MOCK-3333333333333333',
            1000,'RWF','+250781234567','250781234567','+250 781 234 567','mtn','prompt','pending','simulated_pending','not_applied');
        RAISE EXCEPTION 'single pending attempt per order missing';
    EXCEPTION WHEN unique_violation THEN NULL; END;

    BEGIN
        UPDATE payment_attempts SET amount_rwf=2000 WHERE id=aid;
        RAISE EXCEPTION 'immutable authoritative amount missing';
    EXCEPTION WHEN check_violation THEN NULL; END;

    BEGIN
        INSERT INTO payment_attempts(order_id,customer_id,order_reference,provider_name,provider_environment,is_mock,
            idempotency_key,provider_idempotency_key,request_fingerprint,provider_reference,amount_rwf,currency,
            customer_phone_e164,customer_msisdn,customer_phone_display,network,payment_method,state,prompt_status,stock_outcome,completed_at)
        VALUES (oid,cid,'AV-20261123-ABCDEFGHJKLM','mock','test',true,
            'phase08-sql-failed','av-444444444444444444444444',decode(repeat('44',32),'hex'),'MOCK-4444444444444444',
            1000,'RWF','+250781234567','250781234567','+250 781 234 567','mtn','prompt','failed','simulated_rejected','reservation_released',now());
        RAISE EXCEPTION 'failure code requirement missing';
    EXCEPTION WHEN check_violation THEN NULL; END;

    BEGIN
        INSERT INTO payment_attempts(order_id,customer_id,order_reference,provider_name,provider_environment,is_mock,
            idempotency_key,provider_idempotency_key,request_fingerprint,provider_reference,amount_rwf,currency,
            customer_phone_e164,customer_msisdn,customer_phone_display,network,payment_method,state,prompt_status,stock_outcome)
        VALUES (oid,cid,'AV-20261123-ABCDEFGHJKLM','mock','test',true,
            'phase08-sql-succeeded','av-555555555555555555555555',decode(repeat('55',32),'hex'),'MOCK-5555555555555555',
            1000,'RWF','+250781234567','250781234567','+250 781 234 567','mtn','prompt','succeeded','simulated_approved','reservation_consumed');
        RAISE EXCEPTION 'completed_at requirement missing';
    EXCEPTION WHEN check_violation THEN NULL; END;

    BEGIN
        INSERT INTO payment_attempts(order_id,customer_id,order_reference,provider_name,provider_environment,is_mock,
            idempotency_key,provider_idempotency_key,request_fingerprint,provider_reference,amount_rwf,currency,
            customer_phone_e164,customer_msisdn,customer_phone_display,network,payment_method,state,prompt_status,stock_outcome,completed_at)
        VALUES (oid,cid,'AV-20261123-ABCDEFGHJKLM','mock','test',true,
            'phase08-sql-pendingdone','av-666666666666666666666666',decode(repeat('66',32),'hex'),'MOCK-6666666666666666',
            1000,'RWF','+250781234567','250781234567','+250 781 234 567','mtn','prompt','pending','simulated_pending','not_applied',now());
        RAISE EXCEPTION 'pending completion check missing';
    EXCEPTION WHEN check_violation THEN NULL; END;

    BEGIN
        INSERT INTO payment_attempts(order_id,customer_id,order_reference,provider_name,provider_environment,is_mock,
            idempotency_key,provider_idempotency_key,request_fingerprint,provider_reference,amount_rwf,currency,
            customer_phone_e164,customer_msisdn,customer_phone_display,network,payment_method,state,prompt_status,stock_outcome,completed_at)
        VALUES (oid,cid,'AV-20261123-ABCDEFGHJKLM','mock','test',true,
            'phase08-sql-badstate','av-777777777777777777777777',decode(repeat('77',32),'hex'),'MOCK-7777777777777777',
            1000,'RWF','+250781234567','250781234567','+250 781 234 567','mtn','prompt','expired','simulated_timeout','reservation_released',now());
        RAISE EXCEPTION 'payment state check missing';
    EXCEPTION WHEN check_violation THEN NULL; END;

    INSERT INTO payment_events(attempt_id,delivery_id,event_type,provider_name,provider_environment,verification_status,payload)
    VALUES (aid,'init-phase08-sql','payment.initiated','mock','test','not_applicable','{"simulated":true}'::jsonb);
    BEGIN
        INSERT INTO payment_events(attempt_id,delivery_id,event_type,provider_name,provider_environment,verification_status,payload)
        VALUES (aid,'init-phase08-sql','payment.succeeded','mock','test','not_applicable','{}'::jsonb);
        RAISE EXCEPTION 'delivery deduplication missing';
    EXCEPTION WHEN unique_violation THEN NULL; END;

    INSERT INTO payment_transitions(attempt_id,from_state,to_state,event_type,actor,delivery_id,metadata)
    VALUES (aid,NULL,'pending','payment.initiated','customer','init-phase08-sql','{"provider":"mock"}'::jsonb);
    BEGIN
        INSERT INTO payment_transitions(attempt_id,from_state,to_state,event_type,actor,delivery_id,metadata)
        VALUES (aid,'pending','succeeded','payment.hacked','mock_simulation','sim-phase08-sql-hacked','{}'::jsonb);
        RAISE EXCEPTION 'transition event check missing';
    EXCEPTION WHEN check_violation THEN NULL; END;

    INSERT INTO payment_audit_events(attempt_id,order_id,customer_id,action,metadata)
    VALUES (aid,oid,cid,'payment.sql.check','{"simulated":true}'::jsonb);

    BEGIN
        DELETE FROM orders WHERE id=oid;
        RAISE EXCEPTION 'historical order reference protection missing';
    EXCEPTION WHEN restrict_violation THEN NULL; END;
    BEGIN
        DELETE FROM customers WHERE id=cid;
        RAISE EXCEPTION 'historical customer reference protection missing';
    EXCEPTION WHEN restrict_violation THEN NULL; END;

    RAISE NOTICE 'PASS: Phase 08 payment schema, state, uniqueness and audit constraints verified';
END $$;
ROLLBACK;
