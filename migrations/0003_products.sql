-- Phase 03 only. Products are archived, never destructively deleted by the API.
CREATE TABLE products (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 sku TEXT NOT NULL UNIQUE CHECK (sku ~ '^[A-Z0-9][A-Z0-9_-]{2,63}$'),
 name_en TEXT NOT NULL CHECK (char_length(btrim(name_en)) BETWEEN 1 AND 240),
 name_rw TEXT CHECK (char_length(name_rw) <= 240),
 brand TEXT CHECK (char_length(brand) <= 240),
 model TEXT CHECK (char_length(model) <= 240),
 description_en TEXT NOT NULL CHECK (char_length(btrim(description_en)) BETWEEN 1 AND 5000),
 description_rw TEXT NOT NULL CHECK (char_length(btrim(description_rw)) BETWEEN 1 AND 5000),
 category TEXT NOT NULL CHECK (category IN ('phones-and-tablets','computers','gaming','cameras','audio','wearables','accessories')),
 price_rwf BIGINT NOT NULL CHECK (price_rwf BETWEEN 1 AND 9007199254740991),
 comparison_price_rwf BIGINT CHECK (comparison_price_rwf BETWEEN price_rwf AND 9007199254740991),
 condition TEXT NOT NULL CHECK (condition IN ('BRAND_NEW_SEALED','BRAND_NEW_OPEN_BOX','REFURBISHED','USED_GRADE_A','USED_GRADE_B','USED_GRADE_C')),
 country TEXT NOT NULL CHECK (country = 'RW'),
 district TEXT CHECK (char_length(district) <= 240),
 sector TEXT CHECK (char_length(sector) <= 240),
 published BOOLEAN NOT NULL DEFAULT false,
 published_at TIMESTAMPTZ,
 deleted_at TIMESTAMPTZ,
 version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 CHECK (NOT published OR (deleted_at IS NULL AND published_at IS NOT NULL))
);
CREATE INDEX products_public_category_idx ON products(category, created_at DESC, id) WHERE published AND deleted_at IS NULL;
CREATE INDEX products_public_price_idx ON products(price_rwf,id) WHERE published AND deleted_at IS NULL;
CREATE INDEX products_admin_created_idx ON products(created_at DESC,id) WHERE deleted_at IS NULL;

CREATE TABLE inventory (
 product_id UUID PRIMARY KEY REFERENCES products(id) ON DELETE RESTRICT,
 stock_quantity BIGINT NOT NULL CHECK (stock_quantity BETWEEN 0 AND 9007199254740991),
 low_stock_threshold BIGINT NOT NULL CHECK (low_stock_threshold BETWEEN 0 AND 9007199254740991),
 availability TEXT NOT NULL CHECK (availability IN ('available','out-of-stock','unavailable')),
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 CHECK ((availability <> 'available' OR stock_quantity > 0) AND (availability <> 'out-of-stock' OR stock_quantity = 0))
);
CREATE TABLE product_specifications (
 product_id UUID NOT NULL REFERENCES products(id) ON DELETE RESTRICT,
 key TEXT NOT NULL CHECK (char_length(btrim(key)) BETWEEN 1 AND 80 AND lower(key) NOT IN ('__proto__','constructor','prototype')),
 value TEXT NOT NULL CHECK (char_length(btrim(value)) BETWEEN 1 AND 240),
 display_order INTEGER NOT NULL CHECK (display_order BETWEEN 0 AND 18),
 PRIMARY KEY(product_id,key),
 UNIQUE(product_id,display_order)
);
CREATE UNIQUE INDEX product_specifications_key_idx ON product_specifications(product_id,lower(key));
CREATE TABLE product_images (
 id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
 product_id UUID NOT NULL REFERENCES products(id) ON DELETE RESTRICT,
 storage_key TEXT NOT NULL UNIQUE CHECK (storage_key ~ '^[a-f0-9]{64}\.(jpg|png)$'),
 original_filename TEXT NOT NULL CHECK (char_length(original_filename) BETWEEN 1 AND 240),
 mime_type TEXT NOT NULL CHECK (mime_type IN ('image/jpeg','image/png')),
 file_size BIGINT NOT NULL CHECK (file_size BETWEEN 1 AND 5242880),
 content_hash BYTEA NOT NULL CHECK (octet_length(content_hash)=32),
 alt_en TEXT NOT NULL DEFAULT '' CHECK (char_length(alt_en) <= 240),
 alt_rw TEXT NOT NULL DEFAULT '' CHECK (char_length(alt_rw) <= 240),
 is_primary BOOLEAN NOT NULL DEFAULT false,
 display_order INTEGER NOT NULL CHECK (display_order BETWEEN 0 AND 5),
 created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 UNIQUE(product_id,content_hash),
 UNIQUE(product_id,display_order) DEFERRABLE INITIALLY DEFERRED
);
CREATE UNIQUE INDEX product_images_primary_idx ON product_images(product_id) WHERE is_primary;
CREATE INDEX product_images_product_idx ON product_images(product_id,display_order);
