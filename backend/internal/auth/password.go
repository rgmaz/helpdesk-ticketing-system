package auth

import (
	"crypto/rand"
	"errors"
	"fmt"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

// Password validation rules. Length is the dominant factor, so the minimum is
// generous while composition rules stay light enough to be memorable.
const (
	MinPasswordLen = 10
	MaxPasswordLen = 128
)

var (
	ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	ErrPasswordTooLong  = fmt.Errorf("password must be at most %d characters", MaxPasswordLen)
	ErrPasswordWeak     = errors.New("password must include a letter and a digit")
)

// HashPassword produces a bcrypt hash. bcrypt silently truncates beyond 72
// bytes, so the length is validated before hashing.
func HashPassword(plain string) (string, error) {
	if err := ValidatePassword(plain); err != nil {
		return "", err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(h), nil
}

// VerifyPassword reports whether plain matches the stored bcrypt hash.
func VerifyPassword(hash, plain string) bool {
	if hash == "" {
		// Still burn a comparison so missing-hash accounts do not respond
		// measurably faster than wrong-password accounts.
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$invalidinvalidinvalidinvalidinvalidinvalidinvalidinv"), []byte(plain))
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// ValidatePassword enforces length and composition rules.
func ValidatePassword(plain string) error {
	if len(plain) < MinPasswordLen {
		return ErrPasswordTooShort
	}
	if len(plain) > MaxPasswordLen {
		return ErrPasswordTooLong
	}
	var hasLetter, hasDigit bool
	for _, r := range plain {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		case unicode.IsDigit(r):
			hasDigit = true
		}
	}
	if !hasLetter || !hasDigit {
		return ErrPasswordWeak
	}
	return nil
}

// passwordAlphabet excludes characters that are easy to confuse when a generated
// password is transcribed by hand from a log file.
const passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// GeneratePassword returns a cryptographically random password of the requested
// length that satisfies ValidatePassword.
//
// It is used for the bootstrap administrator so the system never starts with a
// well-known default credential that an operator might forget to change.
func GeneratePassword(length int) (string, error) {
	if length < MinPasswordLen {
		length = MinPasswordLen
	}
	if length > MaxPasswordLen {
		length = MaxPasswordLen
	}
	// 6 bits per character from a 57-symbol alphabet is just under 6 bits, so
	// draw one extra byte per character and reject anything outside the range.
	raw := make([]byte, length)
	buf := make([]byte, length)
	for i := range raw {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generate password: %w", err)
		}
		raw[i] = passwordAlphabet[int(buf[i])%len(passwordAlphabet)]
	}
	out := string(raw)
	// Guarantee composition so the result always passes validation.
	if !hasLetter(out) || !hasDigit(out) {
		return GeneratePassword(length)
	}
	return out, nil
}

func hasLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

func hasDigit(s string) bool {
	for _, r := range s {
		if unicode.IsDigit(r) {
			return true
		}
	}
	return false
}
