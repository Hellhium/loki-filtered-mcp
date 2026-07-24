package auth

import (
	"net/http"
	"strings"
	"testing"
)

const (
	tokA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tokB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	tokC = "cccccccccccccccccccccccccccccccc"
)

func testIndex(t *testing.T) *Index[string] {
	t.Helper()
	ix, err := NewIndex([]Entry[string]{
		{Name: "a", Tokens: []string{tokA, tokC}, Value: "instance-a"},
		{Name: "b", Tokens: []string{tokB}, Value: "instance-b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

func TestLookup(t *testing.T) {
	ix := testIndex(t)
	if ix.Len() != 3 {
		t.Errorf("Len = %d, want 3", ix.Len())
	}

	tests := []struct {
		token string
		want  string
		ok    bool
	}{
		{tokA, "instance-a", true},
		{tokC, "instance-a", true}, // second token of the same entry (rotation)
		{tokB, "instance-b", true},
		{"unknown-token-unknown-token-xxxx", "", false},
		{"", "", false},
		{strings.ToUpper(tokA), "", false}, // tokens are case-sensitive
		{tokA + "x", "", false},
		{tokA[:len(tokA)-1], "", false}, // a prefix must not authenticate
	}
	for _, tc := range tests {
		got, ok := ix.Lookup(tc.token)
		if ok != tc.ok || got != tc.want {
			t.Errorf("Lookup(%q) = %q, %t; want %q, %t", tc.token, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNewIndexRejectsDuplicateToken(t *testing.T) {
	_, err := NewIndex([]Entry[string]{
		{Name: "a", Tokens: []string{tokA}, Value: "a"},
		{Name: "b", Tokens: []string{tokA}, Value: "b"},
	})
	if err == nil {
		t.Fatal("expected an error when two entries claim the same token")
	}
	if !strings.Contains(err.Error(), "also used by") {
		t.Errorf("error = %v", err)
	}
}

func TestNewIndexRejectsEmptyToken(t *testing.T) {
	if _, err := NewIndex([]Entry[string]{{Name: "a", Tokens: []string{""}}}); err == nil {
		t.Fatal("expected an error for an empty token")
	}
}

func TestNewIndexEmpty(t *testing.T) {
	ix, err := NewIndex[string](nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ix.Lookup(tokA); ok {
		t.Error("an empty index must authenticate nothing")
	}
}

func TestBearerToken(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
		ok     bool
	}{
		{"standard", "Bearer " + tokA, tokA, true},
		{"lowercase scheme", "bearer " + tokA, tokA, true},
		{"mixed case scheme", "BeArEr " + tokA, tokA, true},
		{"surrounding space", "Bearer   " + tokA + "  ", tokA, true},
		{"absent", "", "", false},
		{"basic auth", "Basic dXNlcjpwYXNz", "", false},
		{"scheme only", "Bearer ", "", false},
		{"no scheme", tokA, "", false},
		{"token shorter than the scheme", "Bear", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := http.NewRequest(http.MethodGet, "http://x/mcp", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			got, ok := BearerToken(r)
			if got != tc.want || ok != tc.ok {
				t.Errorf("BearerToken(%q) = %q, %t; want %q, %t", tc.header, got, ok, tc.want, tc.ok)
			}
		})
	}
}
