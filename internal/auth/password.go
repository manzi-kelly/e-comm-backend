package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	passwordMinLength = 12
	passwordMaxLength = 128

	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024
	argonThreads uint8  = 1
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

var (
	ErrInvalidPasswordHash = errors.New("invalid password hash")
	ErrPasswordTooWeak     = errors.New("password does not meet requirements")
	ErrPasswordTooLong     = errors.New("password is too long")
	ErrPasswordMismatch    = errors.New("password does not match")
)

type argonParams struct {
	time    uint32
	memory  uint32
	threads uint8
	keyLen  uint32
	salt    []byte
	hash    []byte
}

// ValidatePasswordStrength enforces minimum complexity requirements for administrator passwords.
func ValidatePasswordStrength(password string) error {
	if utf8.RuneCountInString(password) > passwordMaxLength {
		return ErrPasswordTooLong
	}
	if len(password) > 4096 {
		return ErrPasswordTooLong
	}
	if utf8.RuneCountInString(password) < passwordMinLength {
		return ErrPasswordTooWeak
	}
	// Reject passwords that are only whitespace or trivially repetitive.
	trimmed := strings.TrimSpace(password)
	if len(trimmed) < passwordMinLength {
		return ErrPasswordTooWeak
	}
	allSame := true
	var first rune
	i := 0
	for _, r := range password {
		if i == 0 {
			first = r
		} else if r != first {
			allSame = false
			break
		}
		i++
	}
	if allSame {
		return ErrPasswordTooWeak
	}
	return nil
}

// HashPassword hashes a password using Argon2id and returns a PHC-encoded string.
func HashPassword(password string) (string, error) {
	if err := ValidatePasswordStrength(password); err != nil {
		return "", err
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64Salt := base64.RawStdEncoding.EncodeToString(salt)
	b64Hash := base64.RawStdEncoding.EncodeToString(hash)
	encoded := fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads, b64Salt, b64Hash)
	return encoded, nil
}

// VerifyPassword verifies a password against a PHC-encoded Argon2id hash in constant time.
func VerifyPassword(password, encoded string) error {
	if utf8.RuneCountInString(password) > passwordMaxLength || len(password) > 4096 {
		return ErrPasswordMismatch
	}
	params, err := decodeHash(encoded)
	if err != nil {
		return err
	}
	derived := argon2.IDKey([]byte(password), params.salt, params.time, params.memory, params.threads, params.keyLen)
	if subtle.ConstantTimeCompare(derived, params.hash) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}

func decodeHash(encoded string) (*argonParams, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		return nil, ErrInvalidPasswordHash
	}
	if parts[1] != "argon2id" {
		return nil, ErrInvalidPasswordHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return nil, ErrInvalidPasswordHash
	}
	params := &argonParams{}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &params.memory, &params.time, &params.threads); err != nil {
		return nil, ErrInvalidPasswordHash
	}
	if params.memory == 0 || params.time == 0 || params.threads == 0 {
		return nil, ErrInvalidPasswordHash
	}
	// Guard against unreasonable parameters during decode.
	if params.memory > math.MaxUint32/2 || params.time > 10 || params.threads > 16 {
		return nil, ErrInvalidPasswordHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, ErrInvalidPasswordHash
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return nil, ErrInvalidPasswordHash
	}
	if len(hash) == 0 || len(salt) == 0 {
		return nil, ErrInvalidPasswordHash
	}
	params.salt = salt
	params.hash = hash
	params.keyLen = uint32(len(hash))
	return params, nil
}
