package product

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Storage is private product-image storage, separate from any future payment proof store.
// Implementations must provide exclusive creation and context-aware bounded operations.
type Storage interface {
	Put(context.Context, string, []byte) error
	Open(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
	Keys(context.Context) ([]string, error)
}
type LocalStorage struct{ root *os.Root }

var keyPattern = regexp.MustCompile(`^[a-f0-9]{64}\.(jpg|png)$`)

func NewLocalStorage(path string) (*LocalStorage, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, ErrStorage
	}
	if err = os.MkdirAll(absolute, 0700); err != nil {
		return nil, ErrStorage
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, ErrStorage
	}
	return &LocalStorage{root: root}, nil
}
func (s *LocalStorage) Close() error { return s.root.Close() }
func (s *LocalStorage) Put(ctx context.Context, key string, b []byte) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !keyPattern.MatchString(key) {
		return ErrValidation
	}
	f, err := s.root.OpenFile(key, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ErrStorage
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = s.root.Remove(key)
		return ErrStorage
	}
	return nil
}
func (s *LocalStorage) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !keyPattern.MatchString(key) {
		return nil, ErrValidation
	}
	f, err := s.root.Open(key)
	if err != nil {
		return nil, ErrStorage
	}
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		_ = f.Close()
		return nil, ErrStorage
	}
	return f, nil
}
func (s *LocalStorage) Delete(ctx context.Context, key string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !keyPattern.MatchString(key) {
		return ErrValidation
	}
	err := s.root.Remove(key)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
func (s *LocalStorage) Keys(ctx context.Context) ([]string, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	f, err := s.root.Open(".")
	if err != nil {
		return nil, ErrStorage
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, ErrStorage
	}
	keys := []string{}
	for _, e := range entries {
		if !e.IsDir() && keyPattern.MatchString(e.Name()) {
			keys = append(keys, e.Name())
		}
	}
	return keys, nil
}

type Upload struct {
	Filename string
	MIME     string
	Data     []byte
	Alt      Description
}
type preparedImage struct {
	Upload
	Hash []byte
	Key  string
}

var decodeSlots = make(chan struct{}, 2)

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if int64(b.Len()+len(p)) > MaxImageBytes {
		return 0, ErrValidation
	}
	return b.Buffer.Write(p)
}
func prepareImage(ctx context.Context, in Upload) (preparedImage, error) {
	select {
	case decodeSlots <- struct{}{}:
		defer func() { <-decodeSlots }()
	case <-ctx.Done():
		return preparedImage{}, ctx.Err()
	}
	if len(in.Data) == 0 || int64(len(in.Data)) > MaxImageBytes || !SafeText(in.Alt.EN, 240, false) || !SafeText(in.Alt.RW, 240, false) {
		return preparedImage{}, ErrValidation
	}
	sniff := http.DetectContentType(in.Data)
	extension := strings.ToLower(filepath.Ext(in.Filename))
	if sniff != in.MIME || !((sniff == "image/png" && extension == ".png") || (sniff == "image/jpeg" && (extension == ".jpg" || extension == ".jpeg"))) {
		return preparedImage{}, ErrUnsupportedImage
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(in.Data))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 8192 || cfg.Height > 8192 || int64(cfg.Width)*int64(cfg.Height) > 16000000 {
		return preparedImage{}, ErrUnsupportedImage
	}
	decoded, actual, err := image.Decode(bytes.NewReader(in.Data))
	if err != nil || actual != format {
		return preparedImage{}, ErrUnsupportedImage
	}
	var out boundedBuffer
	ext := "png"
	if format == "jpeg" {
		ext = "jpg"
		err = jpeg.Encode(&out, decoded, &jpeg.Options{Quality: 90})
	} else if format == "png" {
		err = png.Encode(&out, decoded)
	} else {
		return preparedImage{}, ErrUnsupportedImage
	}
	if err != nil {
		return preparedImage{}, ErrValidation
	}
	random := make([]byte, 32)
	if _, err = rand.Read(random); err != nil {
		return preparedImage{}, err
	}
	// Retain only a short basename as private metadata, never as a storage path.
	name := filepath.Base(strings.ReplaceAll(in.Filename, "\\", "/"))
	if !SafeText(name, 240, true) {
		return preparedImage{}, ErrValidation
	}
	in.Filename = name
	in.Data = out.Bytes()
	hash := sha256.Sum256(in.Data)
	return preparedImage{Upload: in, Hash: hash[:], Key: hex.EncodeToString(random) + "." + ext}, nil
}

// Cleanup uses the same PostgreSQL advisory lock as every upload/removal. There
// is no unsafe file-system sweep concurrent with an uncommitted upload.
func (s *Service) CleanupOrphans(ctx context.Context) (int, error) {
	if s.storage == nil {
		return 0, ErrStorage
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	if err = s.lockImages(ctx, tx); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT storage_key FROM product_images`)
	if err != nil {
		return 0, err
	}
	referenced := map[string]bool{}
	for rows.Next() {
		var k string
		if err = rows.Scan(&k); err != nil {
			rows.Close()
			return 0, err
		}
		referenced[k] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	keys, err := s.storage.Keys(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, k := range keys {
		if !referenced[k] {
			if err = s.storage.Delete(ctx, k); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, tx.Commit(ctx)
}
