-- Isolated PostgreSQL verification for Phase 06: all synthetic rows roll back.
\set ON_ERROR_STOP on
BEGIN;
DO $$
DECLARE
 pid uuid;
 cid uuid;
 oid uuid;
 sid uuid;
 n integer;
BEGIN
 SELECT count(*) INTO n FROM schema_migrations
 WHERE version IN ('0001_foundation.sql','0002_admin_auth.sql','0003_products.sql','0004_customer_orders.sql');
 IF n <> 4 THEN RAISE EXCEPTION 'missing migration records'; END IF;

 INSERT INTO products(sku,name_en,description_en,description_rw,category,price_rwf,condition,country,published,published_at)
 VALUES ('PHASE06-SQL-SKU','Phase06 SQL constraint test','Test','Test','audio',1000,'USED_GRADE_A','RW',true,now())
 RETURNING id INTO pid;
 INSERT INTO inventory(product_id,stock_quantity,low_stock_threshold,availability) VALUES(pid,3,1,'available');

 INSERT INTO customers(name,email,phone_e164,phone_display,password_hash,status)
 VALUES ('Phase06 Test','phase06-sql@example.test','+250781234567','+250 781 234 567','$argon2id$v=19$m=65536,t=3,p=1$c2FsdA$aGFzaA','active')
 RETURNING id INTO cid;

 BEGIN
  INSERT INTO customers(name,email,phone_e164,phone_display,password_hash)
  VALUES ('Bad Email','PHASE06-SQL@example.test','+250781234568','+250 781 234 568','$argon2id$invalid');
  RAISE EXCEPTION 'customer email normalization check missing';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO customers(name,email,phone_e164,phone_display,password_hash)
  VALUES ('Bad Phone','phase06-sql-2@example.test','+254781234568','+254 781 234 568','$argon2id$invalid');
  RAISE EXCEPTION 'Rwanda phone check missing';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO customers(name,email,phone_e164,phone_display,password_hash)
  VALUES ('Bad Hash','phase06-sql-3@example.test','+250781234569','+250 781 234 569','plaintext');
  RAISE EXCEPTION 'Argon2id hash check missing';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO customers(name,email,phone_e164,phone_display,password_hash)
  VALUES ('X','phase06-sql-4@example.test','+250781234570','+250 781 234 570','$argon2id$invalid');
  RAISE EXCEPTION 'customer name length check missing';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO customers(name,email,phone_e164,phone_display,password_hash)
  VALUES ('Duplicate','phase06-sql@example.test','+250781234571','+250 781 234 571','$argon2id$invalid');
  RAISE EXCEPTION 'customer email uniqueness missing';
 EXCEPTION WHEN unique_violation THEN NULL; END;

 INSERT INTO customer_sessions(customer_id,token_hash,expires_at)
 VALUES(cid,decode(repeat('01',32),'hex'),now()+interval '30 minutes') RETURNING id INTO sid;
 BEGIN
  INSERT INTO customer_sessions(customer_id,token_hash,expires_at)
  VALUES(cid,decode(repeat('02',33),'hex'),now()+interval '30 minutes');
  RAISE EXCEPTION 'session token hash length check missing';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO customer_sessions(customer_id,token_hash,expires_at)
  VALUES(cid,decode(repeat('03',32),'hex'),now()-interval '1 minute');
  RAISE EXCEPTION 'session expiry check missing';
 EXCEPTION WHEN check_violation THEN NULL; END;

 INSERT INTO orders(reference,customer_id,idempotency_key,request_fingerprint,customer_name,customer_email,
  customer_phone_e164,customer_phone_display,country,province,district,sector,address_line,
  subtotal_rwf,delivery_fee_rwf,delivery_fee_status,total_rwf)
 VALUES('AV-20261122-ABCDEFGHJKLM',cid,'phase06-sql-idem',decode(repeat('04',32),'hex'),'Phase06 Test','phase06-sql@example.test',
  '+250781234567','+250 781 234 567','RW','Kigali','Gasabo','Remera','KG 123 St',1000,0,'unavailable',1000)
 RETURNING id INTO oid;

 BEGIN
  INSERT INTO orders(reference,customer_id,idempotency_key,request_fingerprint,customer_name,customer_email,
   customer_phone_e164,customer_phone_display,country,province,district,sector,address_line,
   subtotal_rwf,delivery_fee_rwf,delivery_fee_status,total_rwf)
  VALUES('AV-20261122-ABCDEFGHJKLO',cid,'phase06-sql-idem',decode(repeat('05',32),'hex'),'Phase06 Test','phase06-sql@example.test',
   '+250781234567','+250 781 234 567','RW','Kigali','Gasabo','Remera','KG 123 St',1000,0,'unavailable',1000);
  RAISE EXCEPTION 'order idempotency uniqueness missing';
 EXCEPTION WHEN unique_violation THEN NULL; END;
 BEGIN
  INSERT INTO orders(reference,customer_id,idempotency_key,request_fingerprint,customer_name,customer_email,
   customer_phone_e164,customer_phone_display,country,province,district,sector,address_line,
   subtotal_rwf,delivery_fee_rwf,delivery_fee_status,total_rwf)
  VALUES('bad-reference',cid,'phase06-sql-badref',decode(repeat('06',32),'hex'),'Phase06 Test','phase06-sql@example.test',
   '+250781234567','+250 781 234 567','RW','Kigali','Gasabo','Remera','KG 123 St',1000,0,'unavailable',1000);
  RAISE EXCEPTION 'order reference format check missing';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO orders(reference,customer_id,idempotency_key,request_fingerprint,customer_name,customer_email,
   customer_phone_e164,customer_phone_display,country,province,district,sector,address_line,
   subtotal_rwf,delivery_fee_rwf,delivery_fee_status,total_rwf,payment_status)
  VALUES('AV-20261122-ABCDEFGHJKM',cid,'phase06-sql-badstatus',decode(repeat('07',32),'hex'),'Phase06 Test','phase06-sql@example.test',
   '+250781234567','+250 781 234 567','RW','Kigali','Gasabo','Remera','KG 123 St',1000,0,'unavailable',1000,'not-a-status');
  RAISE EXCEPTION 'payment status check missing';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO orders(reference,customer_id,idempotency_key,request_fingerprint,customer_name,customer_email,
   customer_phone_e164,customer_phone_display,country,province,district,sector,address_line,
   subtotal_rwf,delivery_fee_rwf,delivery_fee_status,total_rwf)
  VALUES('AV-20261122-ABCDEFGHJKN',cid,'phase06-sql-total',decode(repeat('08',32),'hex'),'Phase06 Test','phase06-sql@example.test',
   '+250781234567','+250 781 234 567','RW','Kigali','Gasabo','Remera','KG 123 St',1000,0,'unavailable',999);
  RAISE EXCEPTION 'order total equality check missing';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO orders(reference,customer_id,idempotency_key,request_fingerprint,customer_name,customer_email,
   customer_phone_e164,customer_phone_display,country,province,district,sector,address_line,
   subtotal_rwf,delivery_fee_rwf,delivery_fee_status,total_rwf)
  VALUES('AV-20261122-ABCDEFGHJKO',cid,'phase06-sql-fee',decode(repeat('09',32),'hex'),'Phase06 Test','phase06-sql@example.test',
   '+250781234567','+250 781 234 567','RW','Kigali','Gasabo','Remera','KG 123 St',1000,500,'unavailable',1500);
  RAISE EXCEPTION 'delivery fee check missing';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO orders(reference,customer_id,idempotency_key,request_fingerprint,customer_name,customer_email,
   customer_phone_e164,customer_phone_display,country,province,district,sector,address_line,
   subtotal_rwf,delivery_fee_rwf,delivery_fee_status,total_rwf)
  VALUES('AV-20261122-ABCDEFGHJKP',cid,'phase06-sql-fingerprint',NULL,'Phase06 Test','phase06-sql@example.test',
   '+250781234567','+250 781 234 567','RW','Kigali','Gasabo','Remera','KG 123 St',1000,0,'unavailable',1000);
  RAISE EXCEPTION 'idempotency/fingerprint pairing check missing';
 EXCEPTION WHEN check_violation THEN NULL; END;

 INSERT INTO order_items(order_id,product_id,snapshot_name_en,snapshot_sku,snapshot_condition,quantity,unit_price_rwf,line_total_rwf)
 VALUES(oid,pid,'Phase06 Test','PHASE06-SQL-SKU','USED_GRADE_A',2,1000,2000);
 BEGIN
  INSERT INTO order_items(order_id,product_id,snapshot_name_en,snapshot_sku,snapshot_condition,quantity,unit_price_rwf,line_total_rwf)
  VALUES(oid,pid,'Phase06 Test','PHASE06-SQL-SKU','USED_GRADE_A',1,1000,1000);
  RAISE EXCEPTION 'duplicate order item missing';
 EXCEPTION WHEN unique_violation THEN NULL; END;
 BEGIN
  INSERT INTO order_items(order_id,product_id,snapshot_name_en,snapshot_sku,snapshot_condition,quantity,unit_price_rwf,line_total_rwf)
  VALUES(oid,gen_random_uuid(),'Phase06 Test','PHASE06-SQL-SKU2','USED_GRADE_A',2,1000,999);
  RAISE EXCEPTION 'line total check missing';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO order_items(order_id,product_id,snapshot_name_en,snapshot_sku,snapshot_condition,quantity,unit_price_rwf,line_total_rwf)
  VALUES(oid,gen_random_uuid(),'Phase06 Test','PHASE06-SQL-SKU3','USED_GRADE_A',100,1000,100000);
  RAISE EXCEPTION 'item quantity check missing';
 EXCEPTION WHEN check_violation THEN NULL; END;

 INSERT INTO inventory_reservations(product_id,order_id,quantity,status,expires_at)
 VALUES(pid,oid,2,'active',now()+interval '30 minutes');
 BEGIN
  INSERT INTO inventory_reservations(product_id,order_id,quantity,status,expires_at)
  VALUES(pid,oid,1,'active',now()+interval '30 minutes');
  RAISE EXCEPTION 'reservation uniqueness missing';
 EXCEPTION WHEN unique_violation THEN NULL; END;
 BEGIN
  INSERT INTO inventory_reservations(product_id,order_id,quantity,status,expires_at)
  VALUES(pid,gen_random_uuid(),1,'active',now()+interval '30 minutes');
  RAISE EXCEPTION 'reservation order FK missing';
 EXCEPTION WHEN foreign_key_violation THEN NULL; END;
 BEGIN
  INSERT INTO inventory_reservations(product_id,order_id,quantity,status,expires_at)
  VALUES(pid,oid,0,'active',now()+interval '30 minutes');
  RAISE EXCEPTION 'reservation quantity check missing';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO inventory_reservations(product_id,order_id,quantity,status,expires_at)
  VALUES(pid,oid,1,'paid',now()+interval '30 minutes');
  RAISE EXCEPTION 'reservation status check missing';
 EXCEPTION WHEN check_violation THEN NULL; END;

 INSERT INTO order_audit_events(order_id,customer_id,action,metadata)
 VALUES(oid,cid,'order.created','{"reference":"AV-20261122-ABCDEFGHJKLM"}'::jsonb);
 INSERT INTO customer_audit_events(customer_id,action,metadata)
 VALUES(cid,'customer.order.created','{"order_id":"ok"}'::jsonb);

 BEGIN
  DELETE FROM products WHERE id=pid;
  RAISE EXCEPTION 'historical product reference protection missing';
 EXCEPTION WHEN restrict_violation THEN NULL; END;
 BEGIN
  DELETE FROM customers WHERE id=cid;
  RAISE EXCEPTION 'historical customer reference protection missing';
 EXCEPTION WHEN restrict_violation THEN NULL; END;

 RAISE NOTICE 'PASS: Phase 06 customer/session/order/reservation constraints and reference protection verified';
END $$;
ROLLBACK;
