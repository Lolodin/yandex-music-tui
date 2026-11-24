// Package testutil — общие помощники тестов: синусоидальные WAV и кэш с готовыми треками.
package testutil

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"ymusic/internal/api"
	"ymusic/internal/stream"
)

// WAV — синус заданной частоты, 16 бит, 44.1 кГц, стерео.
func WAV(freq, sec float64) []byte {
	const sr = 44100
	n := int(sec * sr)
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(36 + n*4))
	b.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(1))
	w(uint16(2))
	w(uint32(sr))
	w(uint32(sr * 4))
	w(uint16(4))
	w(uint16(16))
	b.WriteString("data")
	w(uint32(n * 4))
	for i := 0; i < n; i++ {
		v := int16(3000 * math.Sin(2*math.Pi*freq*float64(i)/sr))
		w(v)
		w(v)
	}
	return b.Bytes()
}

// Cache создаёт кэш во временном каталоге и кладёт в него треки (по WAV длиной sec на каждый).
func Cache(t *testing.T, tracks []api.Track, sec float64) *stream.Cache {
	t.Helper()
	dir := t.TempDir()
	cache, err := stream.NewCache(dir, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i, tr := range tracks {
		data := WAV(440+110*float64(i), sec)
		part := filepath.Join(dir, "part")
		if err := os.WriteFile(part, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := cache.Commit(part, stream.Meta{Track: tr, Quality: "lossless", Codec: "flac", Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
	}
	return cache
}
