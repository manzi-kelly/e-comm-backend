package product

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	pool    *pgxpool.Pool
	storage Storage
}

func NewService(pool *pgxpool.Pool, storage Storage) *Service {
	return &Service{pool: pool, storage: storage}
}
func (s *Service) StorageEnabled() bool { return s.storage != nil }

type queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

const productJSON = `jsonb_build_object(
 'id',p.id,'sku',p.sku,'name',jsonb_build_object('en',p.name_en,'rw',p.name_rw),
 'brand',p.brand,'model',p.model,'category',p.category,
 'description',jsonb_build_object('en',p.description_en,'rw',p.description_rw),
 'priceRwf',p.price_rwf,'comparisonPriceRwf',p.comparison_price_rwf,'condition',p.condition,
 'location',jsonb_build_object('country',p.country,'district',p.district,'sector',p.sector),
 'stockQuantity',i.stock_quantity,'lowStockThreshold',i.low_stock_threshold,'availability',i.availability,
 'published',p.published,'publishedAt',p.published_at,'createdAt',p.created_at,'updatedAt',p.updated_at,'version',p.version,
 'specifications',COALESCE((SELECT jsonb_object_agg(s.key,s.value ORDER BY s.display_order) FROM product_specifications s WHERE s.product_id=p.id),'{}'::jsonb),
 'photos',COALESCE((SELECT jsonb_agg(jsonb_build_object('id',im.id,'url','/api/v1/products/'||p.id||'/images/'||im.id,
 'alt',jsonb_build_object('en',im.alt_en,'rw',im.alt_rw),'isPrimary',im.is_primary,'displayOrder',im.display_order) ORDER BY im.display_order,im.id)
 FROM product_images im WHERE im.product_id=p.id),'[]'::jsonb))`

func dbError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505", "40001", "40P01":
			return ErrConflict
		case "23514", "23502", "22003", "22P02":
			return ErrValidation
		}
	}
	return err
}
func read(ctx context.Context, q queryer, id string, public bool) (Product, error) {
	var p Product
	var b []byte
	filter := ""
	if public {
		filter = " AND p.published"
	}
	err := q.QueryRow(ctx, `SELECT `+productJSON+` FROM products p JOIN inventory i ON i.product_id=p.id WHERE p.id=$1 AND p.deleted_at IS NULL`+filter, id).Scan(&b)
	if err != nil {
		return p, dbError(err)
	}
	err = json.Unmarshal(b, &p)
	return p, err
}
func (s *Service) Get(ctx context.Context, id string, public bool) (Product, error) {
	if !ValidID(id) {
		return Product{}, ErrNotFound
	}
	return read(ctx, s.pool, id, public)
}
func audit(ctx context.Context, tx pgx.Tx, actor, action, id string) error {
	_, err := tx.Exec(ctx, `INSERT INTO admin_audit_events(admin_user_id,action,metadata) VALUES($1,$2,jsonb_build_object('product_id',$3::text))`, actor, action, id)
	return err
}
func (s *Service) begin(ctx context.Context) (pgx.Tx, error) { return s.pool.Begin(ctx) }
func lockProduct(ctx context.Context, tx pgx.Tx, id string, version int64) error {
	if !ValidID(id) {
		return ErrNotFound
	}
	var actual int64
	err := tx.QueryRow(ctx, `SELECT version FROM products WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&actual)
	if err != nil {
		return dbError(err)
	}
	if actual != version {
		return ErrConflict
	}
	return nil
}
func writeChildren(ctx context.Context, tx pgx.Tx, id string, in Input) error {
	_, err := tx.Exec(ctx, `INSERT INTO inventory(product_id,stock_quantity,low_stock_threshold,availability) VALUES($1,$2,$3,$4)
 ON CONFLICT(product_id) DO UPDATE SET stock_quantity=EXCLUDED.stock_quantity,low_stock_threshold=EXCLUDED.low_stock_threshold,availability=EXCLUDED.availability,updated_at=now()`, id, in.StockQuantity, in.LowStockThreshold, in.Availability)
	if err != nil {
		return dbError(err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM product_specifications WHERE product_id=$1`, id); err != nil {
		return err
	}
	for order, k := range SpecKeys(in) {
		if _, err = tx.Exec(ctx, `INSERT INTO product_specifications(product_id,key,value,display_order) VALUES($1,$2,$3,$4)`, id, k, in.Specifications[k], order); err != nil {
			return dbError(err)
		}
	}
	return nil
}
func (s *Service) Create(ctx context.Context, in Input, actor string) (Product, error) {
	if len(Validate(in)) != 0 {
		return Product{}, ErrValidation
	}
	if in.SKU == "" {
		b := make([]byte, 12)
		if _, err := rand.Read(b); err != nil {
			return Product{}, err
		}
		in.SKU = "AUT-" + strings.ToUpper(hex.EncodeToString(b))
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Product{}, err
	}
	defer tx.Rollback(context.Background())
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO products(sku,name_en,name_rw,brand,model,description_en,description_rw,category,price_rwf,comparison_price_rwf,condition,country,district,sector)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`, in.SKU, in.Name.EN, in.Name.RW, in.Brand, in.Model, in.Description.EN, in.Description.RW, in.Category, in.PriceRWF, in.ComparisonPriceRWF, in.Condition, in.Location.Country, in.Location.District, in.Location.Sector).Scan(&id)
	if err != nil {
		return Product{}, dbError(err)
	}
	if err = writeChildren(ctx, tx, id, in); err != nil {
		return Product{}, err
	}
	if err = audit(ctx, tx, actor, "product.created", id); err != nil {
		return Product{}, err
	}
	p, err := read(ctx, tx, id, false)
	if err != nil {
		return p, err
	}
	return p, dbError(tx.Commit(ctx))
}
func (s *Service) Patch(ctx context.Context, id string, version int64, patch map[string]json.RawMessage, actor string) (Product, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return Product{}, err
	}
	defer tx.Rollback(context.Background())
	if err = lockProduct(ctx, tx, id, version); err != nil {
		return Product{}, err
	}
	current, err := read(ctx, tx, id, false)
	if err != nil {
		return Product{}, err
	}
	in, err := Merge(current.Input, patch)
	if err != nil {
		return Product{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE products SET name_en=$2,name_rw=$3,brand=$4,model=$5,description_en=$6,description_rw=$7,category=$8,price_rwf=$9,comparison_price_rwf=$10,condition=$11,country=$12,district=$13,sector=$14,version=version+1,updated_at=now() WHERE id=$1`, id, in.Name.EN, in.Name.RW, in.Brand, in.Model, in.Description.EN, in.Description.RW, in.Category, in.PriceRWF, in.ComparisonPriceRWF, in.Condition, in.Location.Country, in.Location.District, in.Location.Sector)
	if err != nil {
		return Product{}, dbError(err)
	}
	if err = writeChildren(ctx, tx, id, in); err != nil {
		return Product{}, err
	}
	if err = audit(ctx, tx, actor, "product.updated", id); err != nil {
		return Product{}, err
	}
	p, err := read(ctx, tx, id, false)
	if err != nil {
		return p, err
	}
	return p, dbError(tx.Commit(ctx))
}
func (s *Service) State(ctx context.Context, id string, version int64, action, actor string) (Product, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return Product{}, err
	}
	defer tx.Rollback(context.Background())
	if err = lockProduct(ctx, tx, id, version); err != nil {
		return Product{}, err
	}
	switch action {
	case "publish":
		_, err = tx.Exec(ctx, `UPDATE products SET published=true,published_at=COALESCE(published_at,now()),version=version+1,updated_at=now() WHERE id=$1`, id)
	case "unpublish":
		_, err = tx.Exec(ctx, `UPDATE products SET published=false,version=version+1,updated_at=now() WHERE id=$1`, id)
	case "delete":
		_, err = tx.Exec(ctx, `UPDATE products SET published=false,deleted_at=now(),version=version+1,updated_at=now() WHERE id=$1`, id)
	default:
		return Product{}, ErrValidation
	}
	if err != nil {
		return Product{}, dbError(err)
	}
	if err = audit(ctx, tx, actor, "product."+action, id); err != nil {
		return Product{}, err
	}
	var p Product
	if action != "delete" {
		p, err = read(ctx, tx, id, false)
		if err != nil {
			return p, err
		}
	}
	return p, dbError(tx.Commit(ctx))
}

type Query struct {
	Page, Limit                          int
	Search, Category, Availability, Sort string
	MinPrice, MaxPrice                   *int64
}

func (q Query) Validate() bool {
	return q.Page >= 1 && q.Page <= 1000 && q.Limit >= 1 && q.Limit <= 100 && SafeText(q.Search, 128, false) && (q.Category == "" || Contains(Categories, q.Category)) && (q.Availability == "" || Contains([]string{"available", "out-of-stock", "unavailable"}, q.Availability)) && Contains([]string{"newest", "oldest", "price-asc", "price-desc", "name"}, q.Sort) && (q.MinPrice == nil || (*q.MinPrice >= 0 && *q.MinPrice <= MaxInteger)) && (q.MaxPrice == nil || (*q.MaxPrice >= 0 && *q.MaxPrice <= MaxInteger)) && (q.MinPrice == nil || q.MaxPrice == nil || *q.MinPrice <= *q.MaxPrice)
}
func (s *Service) List(ctx context.Context, q Query, public bool) ([]Product, bool, error) {
	if !q.Validate() {
		return nil, false, ErrValidation
	}
	where := `p.deleted_at IS NULL`
	if public {
		where += ` AND p.published`
	}
	args := []any{}
	add := func(clause string, v any) { args = append(args, v); where += fmt.Sprintf(clause, len(args)) }
	if q.Category != "" {
		add(` AND p.category=$%d`, q.Category)
	}
	if q.Availability != "" {
		add(` AND i.availability=$%d`, q.Availability)
	}
	if q.MinPrice != nil {
		add(` AND p.price_rwf >= $%d`, *q.MinPrice)
	}
	if q.MaxPrice != nil {
		add(` AND p.price_rwf <= $%d`, *q.MaxPrice)
	}
	if q.Search != "" {
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q.Search)
		args = append(args, "%"+escaped+"%")
		n := len(args)
		where += fmt.Sprintf(` AND (p.name_en ILIKE $%d OR p.name_rw ILIKE $%d OR p.brand ILIKE $%d OR p.model ILIKE $%d OR p.sku ILIKE $%d)`, n, n, n, n, n)
	}
	order := map[string]string{"newest": "p.created_at DESC,p.id", "oldest": "p.created_at,p.id", "price-asc": "p.price_rwf,p.id", "price-desc": "p.price_rwf DESC,p.id", "name": "p.name_en,p.id"}[q.Sort]
	args = append(args, q.Limit+1, (q.Page-1)*q.Limit)
	rows, err := s.pool.Query(ctx, `SELECT `+productJSON+` FROM products p JOIN inventory i ON i.product_id=p.id WHERE `+where+` ORDER BY `+order+fmt.Sprintf(` LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	result := []Product{}
	for rows.Next() {
		var b []byte
		var p Product
		if err = rows.Scan(&b); err != nil {
			return nil, false, err
		}
		if err = json.Unmarshal(b, &p); err != nil {
			return nil, false, err
		}
		result = append(result, p)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(result) > q.Limit
	if more {
		result = result[:q.Limit]
	}
	return result, more, nil
}

const imageLock int64 = 0x415554494d4147

func (s *Service) lockImages(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, imageLock)
	return err
}
func (s *Service) Upload(ctx context.Context, id string, version int64, uploads []Upload, actor string) (Product, error) {
	if s.storage == nil {
		return Product{}, ErrStorage
	}
	if len(uploads) < 1 || len(uploads) > MaxImages {
		return Product{}, ErrValidation
	}
	prepared := []preparedImage{}
	for _, u := range uploads {
		p, err := prepareImage(ctx, u)
		if err != nil {
			return Product{}, err
		}
		prepared = append(prepared, p)
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Product{}, err
	}
	defer tx.Rollback(context.Background())
	if err = s.lockImages(ctx, tx); err != nil {
		return Product{}, err
	}
	if err = lockProduct(ctx, tx, id, version); err != nil {
		return Product{}, err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM product_images WHERE product_id=$1`, id).Scan(&count); err != nil {
		return Product{}, err
	}
	if count+len(prepared) > MaxImages {
		return Product{}, ErrValidation
	}
	written := []string{}
	committed := false
	defer func() {
		if !committed {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for _, key := range written {
				_ = s.storage.Delete(cleanup, key)
			}
		}
	}()
	// Append after max(order), not row count: removal may have left sparse orders.
	if _, err = tx.Exec(ctx, `UPDATE product_images im SET display_order=x.n FROM (SELECT id,(row_number() OVER (ORDER BY display_order,id)-1)::integer n FROM product_images WHERE product_id=$1) x WHERE im.id=x.id`, id); err != nil {
		return Product{}, err
	}
	for i, p := range prepared {
		if err = s.storage.Put(ctx, p.Key, p.Data); err != nil {
			return Product{}, err
		}
		written = append(written, p.Key)
		_, err = tx.Exec(ctx, `INSERT INTO product_images(product_id,storage_key,original_filename,mime_type,file_size,content_hash,alt_en,alt_rw,is_primary,display_order) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, id, p.Key, p.Filename, p.MIME, len(p.Data), p.Hash, p.Alt.EN, p.Alt.RW, count == 0 && i == 0, count+i)
		if err != nil {
			return Product{}, dbError(err)
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE products SET version=version+1,updated_at=now() WHERE id=$1`, id); err != nil {
		return Product{}, err
	}
	if err = audit(ctx, tx, actor, "product.images.uploaded", id); err != nil {
		return Product{}, err
	}
	result, err := read(ctx, tx, id, false)
	if err != nil {
		return Product{}, err
	}
	// A commit error may mean the commit succeeded but its acknowledgement was
	// lost. Keep files on that path; the locked orphan reconciler checks DB
	// references before deleting anything.
	committed = true
	if err = tx.Commit(ctx); err != nil {
		return Product{}, dbError(err)
	}
	return result, nil
}
func (s *Service) OrderImages(ctx context.Context, id string, version int64, ids []string, primary, actor string) (Product, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return Product{}, err
	}
	defer tx.Rollback(context.Background())
	if err = lockProduct(ctx, tx, id, version); err != nil {
		return Product{}, err
	}
	current, err := read(ctx, tx, id, false)
	if err != nil {
		return Product{}, err
	}
	if len(ids) != len(current.Photos) || len(ids) > MaxImages || len(ids) == 0 {
		return Product{}, ErrValidation
	}
	seen := map[string]bool{}
	existing := map[string]bool{}
	for _, p := range current.Photos {
		existing[p.ID] = true
	}
	for _, imageID := range ids {
		if !existing[imageID] || seen[imageID] {
			return Product{}, ErrValidation
		}
		seen[imageID] = true
	}
	if !seen[primary] {
		return Product{}, ErrValidation
	}
	if _, err = tx.Exec(ctx, `UPDATE product_images SET is_primary=false WHERE product_id=$1`, id); err != nil {
		return Product{}, err
	}
	for order, imageID := range ids {
		if _, err = tx.Exec(ctx, `UPDATE product_images SET display_order=$3,is_primary=(id=$4::uuid) WHERE product_id=$1 AND id=$2`, id, imageID, order, primary); err != nil {
			return Product{}, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE products SET version=version+1,updated_at=now() WHERE id=$1`, id); err != nil {
		return Product{}, err
	}
	if err = audit(ctx, tx, actor, "product.images.ordered", id); err != nil {
		return Product{}, err
	}
	result, err := read(ctx, tx, id, false)
	if err != nil {
		return Product{}, err
	}
	return result, dbError(tx.Commit(ctx))
}
func (s *Service) RemoveImage(ctx context.Context, id, imageID string, version int64, actor string) (Product, error) {
	if !ValidID(imageID) {
		return Product{}, ErrNotFound
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return Product{}, err
	}
	defer tx.Rollback(context.Background())
	if err = s.lockImages(ctx, tx); err != nil {
		return Product{}, err
	}
	if err = lockProduct(ctx, tx, id, version); err != nil {
		return Product{}, err
	}
	var key string
	err = tx.QueryRow(ctx, `DELETE FROM product_images WHERE product_id=$1 AND id=$2 RETURNING storage_key`, id, imageID).Scan(&key)
	if err != nil {
		return Product{}, dbError(err)
	}
	_, err = tx.Exec(ctx, `UPDATE product_images im SET display_order=x.n,is_primary=CASE WHEN x.has_primary THEN im.is_primary ELSE x.n=0 END FROM (SELECT id,(row_number() OVER (ORDER BY display_order,id)-1)::integer n,bool_or(is_primary) OVER () has_primary FROM product_images WHERE product_id=$1) x WHERE im.id=x.id`, id)
	if err != nil {
		return Product{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE products SET version=version+1,updated_at=now() WHERE id=$1`, id); err != nil {
		return Product{}, err
	}
	if err = audit(ctx, tx, actor, "product.image.removed", id); err != nil {
		return Product{}, err
	}
	p, err := read(ctx, tx, id, false)
	if err != nil {
		return Product{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Product{}, err
	}
	// A failed delete leaves a private orphan retried by CleanupOrphans. Never
	// delete before commit, or roll back a DB reference after deleting its file.
	if s.storage != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.storage.Delete(cleanup, key)
	}
	return p, nil
}
func (s *Service) Image(ctx context.Context, id, imageID string, admin bool) (Photo, error) {
	if !ValidID(id) || !ValidID(imageID) {
		return Photo{}, ErrNotFound
	}
	where := " AND p.published"
	if admin {
		where = ""
	}
	var p Photo
	err := s.pool.QueryRow(ctx, `SELECT im.storage_key,im.mime_type,im.file_size FROM product_images im JOIN products p ON p.id=im.product_id WHERE im.product_id=$1 AND im.id=$2 AND p.deleted_at IS NULL`+where, id, imageID).Scan(&p.Key, &p.MIME, &p.Size)
	return p, dbError(err)
}
func (s *Service) OpenImage(ctx context.Context, p Photo) (io.ReadCloser, error) {
	if s.storage == nil {
		return nil, ErrStorage
	}
	return s.storage.Open(ctx, p.Key)
}
