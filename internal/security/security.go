package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/crypto/argon2"
)

const cardAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func NormalizeCard(value string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(value)) {
		if unicode.IsSpace(r) || r == '-' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func GenerateCard() (string, error) {
	const size = 16
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate card entropy: %w", err)
	}
	encoded := base32.NewEncoding(cardAlphabet).WithPadding(base32.NoPadding).EncodeToString(buf)
	encoded = strings.TrimRight(encoded, "=")
	var groups []string
	for len(encoded) > 0 {
		n := 5
		if len(encoded) < n {
			n = len(encoded)
		}
		groups = append(groups, encoded[:n])
		encoded = encoded[n:]
	}
	return "RC-" + strings.Join(groups, "-"), nil
}

func CardHash(secret []byte, card string) (string, error) {
	if len(secret) < 32 {
		return "", errors.New("CARD_HMAC_SECRET must contain at least 32 bytes")
	}
	card = NormalizeCard(card)
	if len(card) < 26 || !strings.HasPrefix(card, "RC") {
		return "", errors.New("invalid card format")
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(card))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func MaskCard(card string) string {
	card = strings.ToUpper(strings.TrimSpace(card))
	if len(card) < 8 {
		return "RC-****"
	}
	return card[:min(7, len(card))] + "-****-" + card[max(0, len(card)-4):]
}

func NewToken() (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	return token, HashToken(token), nil
}

func HashToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func HashTokenHMAC(secret []byte, token string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

func VerifyTokenHash(expected, token string) bool {
	got := HashToken(token)
	return VerifyHashValue(expected, got)
}

func VerifyHashValue(expected, got string) bool {
	a, errA := hex.DecodeString(expected)
	b, errB := hex.DecodeString(got)
	return errA == nil && errB == nil && hmac.Equal(a, b)
}

func HashPassword(password string) (string, error) {
	if len(password) < 12 {
		return "", errors.New("password must be at least 12 bytes")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	const memory = 65536
	const iterations = 3
	const parallelism = 2
	const keyLen = 32
	hash := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, keyLen)
	return fmt.Sprintf("argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", memory, iterations, parallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" || parts[1] != "v=19" {
		return false
	}
	var memory uint32
	var iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		return false
	}
	if memory < 8*1024 || memory > 256*1024 || iterations < 1 || iterations > 10 || parallelism < 1 || parallelism > 8 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, uint32(len(want)))
	return hmac.Equal(got, want)
}
