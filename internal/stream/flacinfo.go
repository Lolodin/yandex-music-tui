package stream

import (
	"bytes"
	"os"
)

// SourceFormat — параметры исходного файла, прочитанные из заголовка FLAC (STREAMINFO).
type SourceFormat struct {
	SampleRate int
	Channels   int
	Bits       int
}

// headPath — путь к файлу, из которого можно прочитать начало трека.
func (src *Source) headPath() (string, int64) {
	if src.FromCache {
		return src.cachePath, 1 << 30
	}
	src.mu.Lock()
	d := src.dl
	src.mu.Unlock()
	if d == nil {
		return "", 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.path, d.written
}

// Format возвращает разрядность и частоту для FLAC (в том числе FLAC в MP4, если заголовок в начале файла).
func (src *Source) Format() (SourceFormat, bool) {
	if src.Codec != "flac" && src.Codec != "flac-mp4" {
		return SourceFormat{}, false
	}
	p, avail := src.headPath()
	if p == "" || avail < 42 {
		return SourceFormat{}, false
	}
	f, err := os.Open(p)
	if err != nil {
		return SourceFormat{}, false
	}
	defer f.Close()
	head := make([]byte, min(avail, 256<<10))
	n, _ := f.ReadAt(head, 0)
	head = head[:n]

	var si []byte
	switch {
	case bytes.HasPrefix(head, []byte("fLaC")) && len(head) >= 4+4+34:
		si = head[8 : 8+34]
	default:
		// MP4: бокс dfLa = версия/флаги (4 байта) + заголовок блока метаданных (4) + STREAMINFO.
		i := bytes.Index(head, []byte("dfLa"))
		if i < 0 || len(head) < i+4+4+4+34 {
			return SourceFormat{}, false
		}
		si = head[i+12 : i+12+34]
	}
	sr := int(si[10])<<12 | int(si[11])<<4 | int(si[12])>>4
	ch := int(si[12]>>1&7) + 1
	bits := (int(si[12]&1)<<4 | int(si[13])>>4) + 1
	if sr == 0 || bits < 4 {
		return SourceFormat{}, false
	}
	return SourceFormat{SampleRate: sr, Channels: ch, Bits: bits}, true
}
