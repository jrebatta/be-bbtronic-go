package auth

import (
	"bytes"
	"net/http/httptest"
	"testing"
)

func TestNewToken(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 50 {
		tok, hash, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if seen[tok] || len(tok) != 43 || Validate(tok) != nil || !bytes.Equal(hash, Hash(tok)) || len(hash) != 32 {
			t.Fatalf("bad token %q", tok)
		}
		seen[tok] = true
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "abc", "550e8400-e29b-41d4-a716-446655440000", string(make([]byte, 43))} {
		if Validate(bad) == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestBearerFromRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"Basic abc", "", false},
		{"Bearer ", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", tt.header)
		got, ok := BearerFromRequest(r)
		if got != tt.want || ok != tt.ok {
			t.Fatalf("%q -> %q %t", tt.header, got, ok)
		}
	}
}

func TestHashKey_DiffersFromTokenHash(t *testing.T) {
	t.Parallel()
	if bytes.Equal(HashKey("x"), Hash("x")) {
		t.Fatal("idempotency keys must be domain-separated")
	}
}
