package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFlexIDAcceptsNumberAndString(t *testing.T) {
	cases := []struct {
		body string
		want int64
		ok   bool
	}{
		{`{"id": 42, "text": "x"}`, 42, true},
		{`{"id": "42", "text": "x"}`, 42, true},
		{`{"id": " 42 ", "text": "x"}`, 42, true},
		{`{"id": "", "text": "x"}`, 0, false},
		{`{"id": "abc", "text": "x"}`, 0, false},
	}

	for _, tc := range cases {
		var req postUpdateRequest
		err := json.Unmarshal([]byte(tc.body), &req)
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error: %v", tc.body, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: expected an error", tc.body)
		}
		if tc.ok && req.ID.Int64() != tc.want {
			t.Errorf("%s: got %d, want %d", tc.body, req.ID.Int64(), tc.want)
		}
	}
}

func TestValidatePostText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"plain", "hello", "hello", true},
		{"trimmed", "  hello \n", "hello", true},
		{"empty", "", "", false},
		{"spaces", "   \t\n", "", false},
		{"max length", strings.Repeat("я", maxPostTextLen), strings.Repeat("я", maxPostTextLen), true},
		{"too long", strings.Repeat("я", maxPostTextLen+1), "", false},
	}

	for _, tc := range cases {
		got, err := validatePostText(tc.in)
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error: %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
