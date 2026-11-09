package security

import (
	"strings"
	"testing"
)

func TestGeneratedCardUsesHighEntropySafeAlphabet(t *testing.T) {
	card, err := GenerateCard()
	if err != nil {
		t.Fatal(err)
	}
	normalized := NormalizeCard(card)
	if len(normalized) != 28 { // RC prefix plus 26 base32 characters (128 random bits).
		t.Fatalf("normalized card length = %d, want 28", len(normalized))
	}
	if !strings.HasPrefix(card, "RC-") {
		t.Fatalf("card has unexpected prefix: %q", card[:3])
	}
	if strings.ContainsAny(normalized, "01IO") {
		t.Fatalf("card contains ambiguous characters: %s", card)
	}
	if got, err := CardHash([]byte(strings.Repeat("s", 32)), card); err != nil || len(got) != 64 {
		t.Fatalf("CardHash() = %q, %v", got, err)
	}
}

func TestCardHashNormalizationAndKeying(t *testing.T) {
	card, err := GenerateCard()
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte(strings.Repeat("a", 32))
	one, err := CardHash(secret, card)
	if err != nil {
		t.Fatal(err)
	}
	two, err := CardHash(secret, strings.ToLower(strings.ReplaceAll(card, "-", " ")))
	if err != nil {
		t.Fatal(err)
	}
	if one != two {
		t.Fatal("case and separator normalization changed the card index")
	}
	otherKey, _ := CardHash([]byte(strings.Repeat("b", 32)), card)
	if one == otherKey {
		t.Fatal("different HMAC keys produced the same card index")
	}
}

func TestPasswordHashRoundTrip(t *testing.T) {
	password := "long-enough-test-password"
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(hash, password) {
		t.Fatal("valid password was rejected")
	}
	if VerifyPassword(hash, password+"x") {
		t.Fatal("invalid password was accepted")
	}
}
