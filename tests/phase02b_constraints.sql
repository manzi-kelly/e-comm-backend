-- Run only against the freshly created, isolated Phase 02B database.
\set ON_ERROR_STOP on
BEGIN;
DO $$
DECLARE uid uuid; sid uuid; n integer;
BEGIN
 SELECT count(*) INTO n FROM schema_migrations
 WHERE version IN ('0001_foundation.sql','0002_admin_auth.sql');
 IF n <> 2 THEN RAISE EXCEPTION 'migration records missing'; END IF;
 INSERT INTO admin_users(email,password_hash,display_name,role)
 VALUES ('sql-constraints@example.test','test-only-not-a-login-hash','Isolated constraints','admin') RETURNING id INTO uid;
 BEGIN
  INSERT INTO admin_users(email,password_hash,display_name,role)
  VALUES ('SQL-CONSTRAINTS@example.test','test','Duplicate','admin');
  RAISE EXCEPTION 'case insensitive uniqueness missing';
 EXCEPTION WHEN unique_violation THEN NULL; END;
 BEGIN
  UPDATE admin_users SET role='customer' WHERE id=uid;
  RAISE EXCEPTION 'role constraint missing';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  UPDATE admin_users SET email='invalid' WHERE id=uid;
  RAISE EXCEPTION 'email constraint missing';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO admin_sessions(admin_user_id,token_hash,expires_at)
  VALUES (uid,decode('01','hex'),now()-interval '1 hour');
  RAISE EXCEPTION 'expiry constraint missing';
 EXCEPTION WHEN check_violation THEN NULL; END;
 BEGIN
  INSERT INTO admin_sessions(admin_user_id,token_hash,expires_at)
  VALUES ('00000000-0000-0000-0000-000000000000',decode('02','hex'),now()+interval '1 hour');
  RAISE EXCEPTION 'session foreign key missing';
 EXCEPTION WHEN foreign_key_violation THEN NULL; END;
 INSERT INTO admin_sessions(admin_user_id,token_hash,expires_at)
 VALUES (uid,decode('03','hex'),now()+interval '1 hour') RETURNING id INTO sid;
 BEGIN
  INSERT INTO admin_sessions(admin_user_id,token_hash,expires_at)
  VALUES (uid,decode('03','hex'),now()+interval '1 hour');
  RAISE EXCEPTION 'token uniqueness missing';
 EXCEPTION WHEN unique_violation THEN NULL; END;
 BEGIN
  INSERT INTO admin_audit_events(action) VALUES ('');
  RAISE EXCEPTION 'audit action constraint missing';
 EXCEPTION WHEN check_violation THEN NULL; END;
 INSERT INTO admin_audit_events(admin_user_id,action) VALUES(uid,'phase02b.constraint.test');
 -- Subtransaction rollback must restore a revoked session.
 BEGIN
  UPDATE admin_sessions SET revoked_at=now() WHERE id=sid;
  RAISE EXCEPTION 'intentional rollback' USING ERRCODE='P0002';
 EXCEPTION WHEN no_data_found THEN NULL; END;
 IF EXISTS(SELECT 1 FROM admin_sessions WHERE id=sid AND revoked_at IS NOT NULL) THEN
  RAISE EXCEPTION 'rollback failed';
 END IF;
 RAISE NOTICE 'PASS: migration records, unique email/token, role/email/expiry/audit checks, session FK, transaction rollback';
END $$;
ROLLBACK;
