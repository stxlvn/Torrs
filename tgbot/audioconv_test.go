package tgbot

import (
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"torrsru/tgbot/torr"
)

func needFFmpeg(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s не установлен", bin)
		}
	}
}

func makeSource(t *testing.T, path string, args ...string) {
	t.Helper()
	full := append([]string{"-hide_banner", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=30",
		"-ac", "2"}, args...)
	full = append(full, path)
	if out, err := exec.Command("ffmpeg", full...).CombinedOutput(); err != nil {
		t.Skipf("ffmpeg не может создать %s: %v %s", filepath.Base(path), err, out)
	}
}

func TestTranscodeCutAllFormats(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()
	sources := []struct {
		name string
		args []string
	}{
		{"src.flac", []string{"-c:a", "flac"}},
		{"src24.flac", []string{"-c:a", "flac", "-sample_fmt", "s32", "-bits_per_raw_sample", "24", "-ar", "96000"}},
		{"src.m4a", []string{"-c:a", "alac"}},
		{"src.wv", []string{"-c:a", "wavpack"}},
		{"src.tta", []string{"-c:a", "tta"}},
		{"src.wav", []string{"-c:a", "pcm_s16le"}},
		{"src24.wav", []string{"-c:a", "pcm_s24le"}},
		{"src32.wav", []string{"-c:a", "pcm_s32le"}},
		{"srcf.wav", []string{"-c:a", "pcm_f32le"}},
		{"src.aiff", []string{"-c:a", "pcm_s16be"}},
		{"src.mp3", []string{"-c:a", "libmp3lame", "-b:a", "192k"}},
		{"srcaac.m4a", []string{"-c:a", "aac", "-b:a", "192k"}},
		{"src.ogg", []string{"-c:a", "libvorbis"}},
		{"src.opus", []string{"-c:a", "libopus"}},
		{"src.wma", []string{"-c:a", "wmav2"}},
	}
	for _, s := range sources {
		src := filepath.Join(dir, s.name)
		makeSource(t, src, s.args...)
		p, err := probeAudio(src)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		modes := []outMode{lossyMode(p)}
		if p.Lossless() {
			modes = []outMode{outFLAC, outALAC}
		}
		for _, m := range modes {
			out := filepath.Join(dir, "cut_"+s.name+m.ext())
			tags := map[string]string{"title": "テスト 曲 №1", "artist": "Исполнитель", "track": "2/9"}
			if err := transcodeAudio(src, out, 10*time.Second, 20*time.Second, p, m, tags); err != nil {
				t.Errorf("%s -> %s: %v", s.name, m.ext(), err)
				continue
			}
			op, err := probeAudio(out)
			if err != nil {
				t.Errorf("%s -> %s: probe: %v", s.name, m.ext(), err)
				continue
			}
			if d := op.Duration - 10*time.Second; d < -150*time.Millisecond || d > 150*time.Millisecond {
				t.Errorf("%s -> %s: длительность %v, ожидалось ~10s", s.name, m.ext(), op.Duration)
			}
			if op.Tags["title"] != "テスト 曲 №1" || op.Tags["artist"] != "Исполнитель" {
				t.Errorf("%s -> %s: теги %v", s.name, m.ext(), op.Tags)
			}
			t.Logf("%-11s %-8s lossless=%-5v -> %s %s %d бит %.3fs", s.name, p.Codec, p.Lossless(), m.ext(), op.Codec, op.BitsRaw, op.Duration.Seconds())
		}

		last := filepath.Join(dir, "last_"+s.name+modes[0].ext())
		if err := transcodeAudio(src, last, 25*time.Second, 0, p, modes[0], map[string]string{"title": "x"}); err != nil {
			t.Errorf("%s: последний трек до конца файла: %v", s.name, err)
		} else if op, _ := probeAudio(last); op == nil || op.Duration < 4800*time.Millisecond {
			t.Errorf("%s: последний трек обрезан: %+v", s.name, op)
		}
	}
}

func TestEmbeddedCueAndChapters(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()

	cue := "PERFORMER \"坂本龍一\"\nTITLE \"戦場のメリークリスマス\"\nFILE \"x.wav\" WAVE\n  TRACK 01 AUDIO\n    TITLE \"Merry Christmas Mr. Lawrence\"\n    INDEX 01 00:00:00\n  TRACK 02 AUDIO\n    TITLE \"Batavia\"\n    INDEX 01 00:12:00\n"
	flac := filepath.Join(dir, "img.flac")
	makeSource(t, flac, "-c:a", "flac", "-metadata", "CUESHEET="+cue)
	p, err := probeAudio(flac)
	if err != nil {
		t.Fatal(err)
	}
	sheet, src := embeddedCue(p)
	if sheet == nil || len(sheet.Files[0].Tracks) != 2 || sheet.Performer != "坂本龍一" {
		t.Fatalf("вшитый CUESHEET не прочитан: %v %+v", src, sheet)
	}

	meta := filepath.Join(dir, "chapters.txt")
	os.WriteFile(meta, []byte(";FFMETADATA1\nalbum=Mix\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=10000\ntitle=Часть 1\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=10000\nEND=30000\ntitle=第二部\n"), 0o644)
	m4a := filepath.Join(dir, "mix.m4a")
	out, err := exec.Command("ffmpeg", "-hide_banner", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "sine=duration=30", "-i", meta, "-map_metadata", "1", "-map_chapters", "1", "-c:a", "aac", m4a).CombinedOutput()
	if err != nil {
		t.Fatalf("m4a с главами: %v %s", err, out)
	}
	p, err = probeAudio(m4a)
	if err != nil {
		t.Fatal(err)
	}
	sheet, src = embeddedCue(p)
	if sheet == nil || len(sheet.Files[0].Tracks) != 2 || sheet.Files[0].Tracks[1].Title != "第二部" || sheet.Files[0].Tracks[1].Start != 10*time.Second {
		t.Fatalf("главы не прочитаны: %v %+v", src, sheet)
	}
}

func writeTestDSF(t *testing.T, path string, secs int) {
	t.Helper()
	const fs, block = 2822400, 4096
	n := fs * secs
	var chans [][]byte
	for _, freq := range []float64{440, 660} {
		bits := make([]byte, n/8)
		integ, y := 0.0, 0.0
		for i := 0; i < n; i++ {
			integ += 0.5*math.Sin(2*math.Pi*freq*float64(i)/fs) - y
			if integ >= 0 {
				y = 1
				bits[i/8] |= 1 << (i % 8)
			} else {
				y = -1
			}
		}
		chans = append(chans, bits)
	}
	var data []byte
	for b := 0; b*block < len(chans[0]); b++ {
		for _, ch := range chans {
			chunk := make([]byte, block)
			copy(chunk, ch[b*block:])
			data = append(data, chunk...)
		}
	}
	le := binary.LittleEndian
	var buf []byte
	buf = append(buf, "DSD "...)
	buf = le.AppendUint64(buf, 28)
	buf = le.AppendUint64(buf, uint64(28+52+12+len(data)))
	buf = le.AppendUint64(buf, 0)
	buf = append(buf, "fmt "...)
	buf = le.AppendUint64(buf, 52)
	for _, v := range []uint32{1, 0, 2, 2, fs, 1} {
		buf = le.AppendUint32(buf, v)
	}
	buf = le.AppendUint64(buf, uint64(n))
	buf = le.AppendUint32(buf, block)
	buf = le.AppendUint32(buf, 0)
	buf = append(buf, "data"...)
	buf = le.AppendUint64(buf, uint64(12+len(data)))
	buf = append(buf, data...)
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDSDConversion(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "track.dsf")
	writeTestDSF(t, src, 3)
	p, err := probeAudio(src)
	if err != nil {
		t.Fatal(err)
	}
	if !p.IsDSD() || !p.Lossless() {
		t.Fatalf("DSF не распознан как DSD: %+v", p)
	}
	for _, m := range []outMode{outFLAC, outALAC} {
		out := filepath.Join(dir, "track"+m.ext())
		if err := transcodeAudio(src, out, 0, 0, p, m, nil); err != nil {
			t.Fatalf("%s: %v", m.ext(), err)
		}
		op, err := probeAudio(out)
		if err != nil {
			t.Fatal(err)
		}
		if op.SampleRate != 88200 || op.BitsRaw != 24 || op.Duration < 2900*time.Millisecond {
			t.Errorf("%s: %d Гц %d бит %v", m.ext(), op.SampleRate, op.BitsRaw, op.Duration)
		}
		vol, _ := exec.Command("ffmpeg", "-hide_banner", "-nostdin", "-i", out, "-af", "highpass=f=200,lowpass=f=2000,volumedetect", "-f", "null", "-").CombinedOutput()
		if !strings.Contains(string(vol), "mean_volume: -") {
			t.Fatalf("volumedetect: %s", vol)
		}
		i := strings.Index(string(vol), "mean_volume: ")
		t.Logf("%s: %d Гц, %d бит, %.2fs, %s", m.ext(), op.SampleRate, op.BitsRaw, op.Duration.Seconds(), strings.Fields(string(vol)[i:])[1]+" dB в полосе тона")
	}
}

func TestSACDImageDetection(t *testing.T) {
	dir := t.TempDir()
	sacd := filepath.Join(dir, "disc.iso")
	buf := make([]byte, 511*2048)
	copy(buf[510*2048:], "SACDMTOC")
	os.WriteFile(sacd, buf, 0o644)
	other := filepath.Join(dir, "office.iso")
	os.WriteFile(other, make([]byte, 511*2048), 0o644)
	if !torr.IsSACDImage(sacd) || torr.IsSACDImage(other) {
		t.Fatal("неверное определение SACD-образа")
	}
}
