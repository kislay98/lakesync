package secrets

import (
	"bytes"
	"testing"
)

func TestSealOpen(t *testing.T) {
	key, err := ParseKey(GenerateKey())
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewAESGCM(key)
	if err != nil {
		t.Fatal(err)
	}
	pt := []byte(`{"uri":"mongodb+srv://u:p@h/db"}`)
	ct, err := s.Seal(pt)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("mongodb")) {
		t.Fatal("ciphertext leaks plaintext")
	}
	ct2, _ := s.Seal(pt)
	if bytes.Equal(ct, ct2) {
		t.Fatal("nonce reuse: identical ciphertexts")
	}
	got, err := s.Open(ct)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("open: %v %q", err, got)
	}
	ct[len(ct)-1] ^= 1
	if _, err := s.Open(ct); err == nil {
		t.Fatal("tampered ciphertext opened")
	}
	other, _ := NewAESGCM(bytes.Repeat([]byte{7}, 32))
	if _, err := other.Open(ct2); err == nil {
		t.Fatal("opened with wrong key")
	}
}

func TestParseKey(t *testing.T) {
	if _, err := ParseKey(""); err == nil {
		t.Error("empty key accepted")
	}
	if _, err := ParseKey("short"); err == nil {
		t.Error("short key accepted")
	}
	if k, err := ParseKey("0123456789abcdef0123456789abcdef"); err != nil || len(k) != 32 {
		t.Errorf("raw 32-char key: %v", err)
	}
	if k, err := ParseKey("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="); err != nil || len(k) != 32 {
		t.Errorf("base64 key: %v", err)
	}
	if _, err := NewAESGCM([]byte("too short")); err == nil {
		t.Error("short key accepted by NewAESGCM")
	}
}
