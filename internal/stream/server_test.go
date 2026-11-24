package stream

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"ymusic/internal/api"
)

func encrypt(key, plain []byte) []byte {
	block, _ := aes.NewCipher(key)
	out := make([]byte, len(plain))
	cipher.NewCTR(block, make([]byte, 16)).XORKeyStream(out, plain)
	return out
}

func TestCtrAt(t *testing.T) {
	key := []byte("0123456789abcdef")
	plain := make([]byte, 10000)
	rand.New(rand.NewSource(1)).Read(plain)
	enc := encrypt(key, plain)
	for _, off := range []int64{0, 1, 15, 16, 17, 4095, 9999} {
		s, err := ctrAt(hex.EncodeToString(key), off)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(enc)-int(off))
		s.XORKeyStream(got, enc[off:])
		if !bytes.Equal(got, plain[off:]) {
			t.Fatalf("offset %d: расшифровка не совпала", off)
		}
	}
}

func TestParseRange(t *testing.T) {
	cases := []struct {
		h          string
		start, end int64
		ok         bool
	}{
		{"", 0, 99, true},
		{"bytes=0-", 0, 99, true},
		{"bytes=10-19", 10, 19, true},
		{"bytes=90-200", 90, 99, true},
		{"bytes=-10", 90, 99, true},
		{"bytes=100-", 0, 0, false},
		{"bytes=5-1", 0, 0, false},
	}
	for _, c := range cases {
		s, e, _, ok := parseRange(c.h, 100)
		if ok != c.ok || (ok && (s != c.start || e != c.end)) {
			t.Errorf("%q: got %d-%d ok=%v", c.h, s, e, ok)
		}
	}
}

// slowCDN отдаёт файл с поддержкой Range, искусственно замедляя полную загрузку.
func slowCDN(data []byte, ranged *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			ranged.Add(1)
			http.ServeContent(w, r, "f", time.Time{}, bytes.NewReader(data))
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.WriteHeader(200)
		for i := 0; i < len(data); i += 64 << 10 {
			end := min(i+64<<10, len(data))
			if _, err := w.Write(data[i:end]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
}

func TestServeEncrypted(t *testing.T) {
	key := []byte("fedcba9876543210")
	plain := make([]byte, 3<<20)
	rand.New(rand.NewSource(2)).Read(plain)
	copy(plain, []byte("fLaC"))
	var ranged atomic.Int32
	cdn := slowCDN(encrypt(key, plain), &ranged)
	defer cdn.Close()

	dir := t.TempDir()
	cache, err := NewCache(dir, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(nil, cache)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	tr := api.Track{ID: "42", Title: "test"}
	src := srv.register(&Source{
		Track: tr, Quality: "lossless", Codec: "flac", srv: srv, requested: "lossless",
		info:   &api.DownloadInfo{URL: cdn.URL, Transport: "encraw", Key: hex.EncodeToString(key), Codec: "flac", Quality: "lossless"},
		infoAt: time.Now(),
	})

	get := func(rng string) []byte {
		req, _ := http.NewRequest("GET", src.URL(), nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return b
	}

	// Далёкий диапазон (как поиск moov в конце MP4) — должен прийти сразу с CDN, а не ждать загрузки.
	t0 := time.Now()
	tail := get(fmt.Sprintf("bytes=%d-", len(plain)-1000))
	if !bytes.Equal(tail, plain[len(plain)-1000:]) {
		t.Fatal("хвост файла не совпал")
	}
	if time.Since(t0) > 500*time.Millisecond || ranged.Load() == 0 {
		t.Fatalf("хвост шёл через последовательную загрузку (%v, range-запросов %d)", time.Since(t0), ranged.Load())
	}
	// Середина и весь файл из растущего временного файла.
	if mid := get("bytes=100000-200000"); !bytes.Equal(mid, plain[100000:200001]) {
		t.Fatal("середина не совпала")
	}
	if full := get(""); !bytes.Equal(full, plain) {
		t.Fatal("файл целиком не совпал")
	}
	// После загрузки файл должен оказаться в кэше.
	deadline := time.Now().Add(5 * time.Second)
	for !src.Committed() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	e, ok := cache.Lookup("42", "hq")
	if !ok {
		t.Fatal("трек не попал в кэш")
	}
	data, _ := os.ReadFile(e.Path)
	if !bytes.Equal(data, plain) {
		t.Fatal("в кэше лежит не то")
	}
	if _, ok := cache.Lookup("42", "lossless"); !ok {
		t.Fatal("lossless не найден")
	}
	// Повторный Prepare берёт файл из кэша без обращения к API (api == nil).
	s2, err := srv.Prepare(context.Background(), tr, "hq")
	if err != nil || !s2.FromCache {
		t.Fatalf("Prepare из кэша: %v %+v", err, s2)
	}
	if got := get2(t, s2.URL()); !bytes.Equal(got, plain) {
		t.Fatal("отдача из кэша не совпала")
	}
}

func get2(t *testing.T, url string) []byte {
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b
}

func TestFlacFormat(t *testing.T) {
	// STREAMINFO: 96 кГц, 2 канала, 24 бита.
	si := make([]byte, 34)
	sr, ch, bps := 96000, 2, 24
	si[10] = byte(sr >> 12)
	si[11] = byte(sr >> 4)
	si[12] = byte(sr&0xF)<<4 | byte(ch-1)<<1 | byte((bps-1)>>4)
	si[13] = byte((bps-1)&0xF) << 4
	head := append([]byte("fLaC\x00\x00\x00\x22"), si...)
	p := t.TempDir() + "/x.flac"
	os.WriteFile(p, head, 0o644)
	src := &Source{Codec: "flac", FromCache: true, cachePath: p}
	f, ok := src.Format()
	if !ok || f.SampleRate != 96000 || f.Channels != 2 || f.Bits != 24 {
		t.Fatalf("got %+v ok=%v", f, ok)
	}
}
