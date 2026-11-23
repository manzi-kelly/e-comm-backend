-- Isolated PostgreSQL verification only: all synthetic rows roll back.
\set ON_ERROR_STOP on
BEGIN;
DO $$
DECLARE pid uuid; n integer;
BEGIN
 SELECT count(*) INTO n FROM schema_migrations WHERE version IN ('0001_foundation.sql','0002_admin_auth.sql','0003_products.sql');
 IF n <> 3 THEN RAISE EXCEPTION 'missing migration records'; END IF;
 INSERT INTO products(sku,name_en,description_en,description_rw,category,price_rwf,condition,country)
 VALUES ('PHASE03-SQL-CONSTRAINTS','Isolated constraint test','Test','Test','audio',100,'USED_GRADE_A','RW') RETURNING id INTO pid;
 INSERT INTO inventory(product_id,stock_quantity,low_stock_threshold,availability) VALUES(pid,3,2,'available');
 BEGIN UPDATE products SET price_rwf=0 WHERE id=pid; RAISE EXCEPTION 'price check missing'; EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN UPDATE products SET comparison_price_rwf=1 WHERE id=pid; RAISE EXCEPTION 'comparison check missing'; EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN UPDATE products SET condition='' WHERE id=pid; RAISE EXCEPTION 'condition check missing'; EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN UPDATE products SET category='cars' WHERE id=pid; RAISE EXCEPTION 'category check missing'; EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN UPDATE products SET country='US' WHERE id=pid; RAISE EXCEPTION 'country check missing'; EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN UPDATE inventory SET stock_quantity=-1 WHERE product_id=pid; RAISE EXCEPTION 'stock check missing'; EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN UPDATE inventory SET stock_quantity=0 WHERE product_id=pid; RAISE EXCEPTION 'availability consistency missing'; EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN UPDATE inventory SET low_stock_threshold=-1 WHERE product_id=pid; RAISE EXCEPTION 'threshold check missing'; EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN DELETE FROM products WHERE id=pid; RAISE EXCEPTION 'historical reference protection missing'; EXCEPTION WHEN restrict_violation THEN NULL; END;
 INSERT INTO product_specifications(product_id,key,value,display_order) VALUES(pid,'ram','8 GB',0);
 BEGIN INSERT INTO product_specifications(product_id,key,value,display_order) VALUES(pid,'RAM','16 GB',1); RAISE EXCEPTION 'spec key uniqueness missing'; EXCEPTION WHEN unique_violation THEN NULL; END;
 INSERT INTO product_images(product_id,storage_key,original_filename,mime_type,file_size,content_hash,is_primary,display_order)
 VALUES(pid,repeat('a',64)||'.png','test.png','image/png',10,decode(repeat('01',32),'hex'),true,0);
 BEGIN
  INSERT INTO product_images(product_id,storage_key,original_filename,mime_type,file_size,content_hash,is_primary,display_order)
  VALUES(pid,repeat('b',64)||'.png','test2.png','image/png',10,decode(repeat('02',32),'hex'),true,1);
  RAISE EXCEPTION 'primary uniqueness missing';
 EXCEPTION WHEN unique_violation THEN NULL; END;
 BEGIN UPDATE product_images SET display_order=6 WHERE product_id=pid; RAISE EXCEPTION 'image limit missing'; EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN UPDATE product_images SET storage_key='../outside.png' WHERE product_id=pid; RAISE EXCEPTION 'unsafe storage key accepted'; EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN UPDATE products SET published=true WHERE id=pid; RAISE EXCEPTION 'publication timestamp check missing'; EXCEPTION WHEN check_violation THEN NULL; END;
 RAISE NOTICE 'PASS: all three migrations, RWF, category, condition, RW, inventory, deletion protection, specifications, image primary/count/key and publication constraints';
END $$;
ROLLBACK;
