package api

import (
	"encoding/json"
	"testing"
)

func TestSignFileInfo(t *testing.T) {
	// Эталон посчитан по алгоритму из yandex-music-api (utils/sign_request.py).
	got := signFileInfo(1700000000, "12345", "lossless", fileInfoCodecs, "encraw")
	if want := "ldTWNQuyZT1VGr65FcGCqeoTZGmm1E3dUzNfLzUUN3Q"; got != want {
		t.Fatalf("sign = %s, want %s", got, want)
	}
}

func TestIDUnmarshal(t *testing.T) {
	var v struct {
		A ID `json:"a"`
		B ID `json:"b"`
	}
	if err := json.Unmarshal([]byte(`{"a": 123, "b": "456"}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.A != "123" || v.B != "456" {
		t.Fatalf("got %q %q", v.A, v.B)
	}
}

func TestExtractToken(t *testing.T) {
	cases := map[string]string{
		"  y0_abc  ": "y0_abc",
		"https://music.yandex.ru/#access_token=y0_xyz&token_type=bearer&expires_in=31535645": "y0_xyz",
	}
	for in, want := range cases {
		if got := ExtractToken(in); got != want {
			t.Errorf("ExtractToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseError(t *testing.T) {
	err := parseError(400, []byte(`{"invocationInfo":{},"error":{"name":"validate","message":"bad sign"}}`))
	if e, ok := err.(*Error); !ok || e.Name != "validate" || e.Message != "bad sign" {
		t.Fatalf("got %#v", err)
	}
}
