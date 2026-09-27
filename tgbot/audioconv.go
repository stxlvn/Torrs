package tgbot

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type audioProbe struct {
	Codec      string
	SampleFmt  string
	SampleRate int
	BitsRaw    int
	Duration   time.Duration
	Tags       map[string]string
	Chapters   []probeChapter
}

type probeChapter struct {
	Start, End time.Duration
	Title      string
}

func probeAudio(path string) (*audioProbe, error) {
	out, err := exec.Command("ffprobe", "-v", "error",
		"-select_streams", "a:0",
		"-show_entries", "stream=codec_name,sample_fmt,sample_rate,bits_per_raw_sample,bits_per_sample:stream_tags:format=duration:format_tags",
		"-show_chapters", "-of", "json", path).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	var raw struct {
		Streams []struct {
			CodecName     string            `json:"codec_name"`
			SampleFmt     string            `json:"sample_fmt"`
			SampleRate    string            `json:"sample_rate"`
			BitsRaw       string            `json:"bits_per_raw_sample"`
			BitsPerSample int               `json:"bits_per_sample"`
			Tags          map[string]string `json:"tags"`
		} `json:"streams"`
		Format struct {
			Duration string            `json:"duration"`
			Tags     map[string]string `json:"tags"`
		} `json:"format"`
		Chapters []struct {
			StartTime string            `json:"start_time"`
			EndTime   string            `json:"end_time"`
			Tags      map[string]string `json:"tags"`
		} `json:"chapters"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("ffprobe json: %w", err)
	}
	p := &audioProbe{Tags: map[string]string{}}
	for k, v := range raw.Format.Tags {
		p.Tags[strings.ToLower(k)] = v
	}
	if len(raw.Streams) > 0 {
		s := raw.Streams[0]
		p.Codec, p.SampleFmt = s.CodecName, s.SampleFmt
		p.SampleRate, _ = strconv.Atoi(s.SampleRate)
		p.BitsRaw, _ = strconv.Atoi(s.BitsRaw)
		if p.BitsRaw == 0 {
			p.BitsRaw = s.BitsPerSample
		}
		for k, v := range s.Tags {
			if _, ok := p.Tags[strings.ToLower(k)]; !ok {
				p.Tags[strings.ToLower(k)] = v
			}
		}
	}
	p.Duration = parseSeconds(raw.Format.Duration)
	for _, ch := range raw.Chapters {
		title := ""
		for k, v := range ch.Tags {
			if strings.EqualFold(k, "title") {
				title = v
			}
		}
		p.Chapters = append(p.Chapters, probeChapter{Start: parseSeconds(ch.StartTime), End: parseSeconds(ch.EndTime), Title: title})
	}
	return p, nil
}

func parseSeconds(s string) time.Duration {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}

var losslessCodecs = map[string]bool{
	"flac": true, "alac": true, "ape": true, "wavpack": true, "tta": true, "tak": true,
	"shorten": true, "mlp": true, "truehd": true, "wmalossless": true, "ralf": true,
}

func (p *audioProbe) Lossless() bool {
	return losslessCodecs[p.Codec] || strings.HasPrefix(p.Codec, "pcm_") || p.IsDSD()
}

func (p *audioProbe) IsDSD() bool {
	return strings.HasPrefix(p.Codec, "dsd_") || p.Codec == "dst"
}

type outMode int

const (
	outFLAC outMode = iota
	outALAC
	outMP3Copy
	outAACCopy
	outAAC
)

func (m outMode) ext() string {
	switch m {
	case outFLAC:
		return ".flac"
	case outMP3Copy:
		return ".mp3"
	}
	return ".m4a"
}

// lossyMode — куда отправлять lossy-источник без повторного сжатия там, где
// это возможно (mp3/aac копируются как есть), остальное — в AAC 256k,
// который Telegram проигрывает как музыку.
func lossyMode(p *audioProbe) outMode {
	switch p.Codec {
	case "mp3":
		return outMP3Copy
	case "aac":
		return outAACCopy
	}
	return outAAC
}

func pcmFixArgs(p *audioProbe, m outMode) []string {
	if m != outFLAC && m != outALAC {
		return nil
	}
	var args []string
	if p.IsDSD() {
		rate := "88200"
		if p.SampleRate > 352800 {
			rate = "176400"
		}
		args = append(args, "-ar", rate)
	}
	s32, s16 := "s32", "s16"
	if m == outALAC {
		s32, s16 = "s32p", "s16p"
	}
	switch {
	case p.IsDSD(), strings.HasPrefix(p.SampleFmt, "flt"), strings.HasPrefix(p.SampleFmt, "dbl"), p.BitsRaw > 24:
		args = append(args, "-sample_fmt", s32, "-bits_per_raw_sample", "24")
	case strings.HasPrefix(p.SampleFmt, "u8"):
		args = append(args, "-sample_fmt", s16)
	}
	return args
}

func codecArgs(m outMode) []string {
	switch m {
	case outFLAC:
		return []string{"-c:a", "flac", "-compression_level", "5"}
	case outALAC:
		return []string{"-c:a", "alac", "-movflags", "+faststart"}
	case outMP3Copy:
		return []string{"-c:a", "copy", "-id3v2_version", "3"}
	case outAACCopy:
		return []string{"-c:a", "copy", "-movflags", "+faststart"}
	default:
		return []string{"-c:a", "aac", "-b:a", "256k", "-movflags", "+faststart"}
	}
}

func ffSeconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 6, 64)
}

// transcodeAudio пишет [start, end) источника (end==0 — до конца) в outPath.
// tags==nil — теги источника сохраняются, иначе заменяются переданными.
func transcodeAudio(src, outPath string, start, end time.Duration, p *audioProbe, m outMode, tags map[string]string) error {
	args := []string{"-hide_banner", "-nostdin", "-y"}
	if start > 0 {
		args = append(args, "-ss", ffSeconds(start))
	}
	if end > 0 {
		args = append(args, "-to", ffSeconds(end))
	}
	args = append(args, "-i", src, "-map", "0:a:0", "-vn")
	if tags != nil {
		args = append(args, "-map_metadata", "-1")
		for k, v := range tags {
			if v != "" {
				args = append(args, "-metadata", k+"="+v)
			}
		}
	}
	args = append(args, codecArgs(m)...)
	args = append(args, pcmFixArgs(p, m)...)
	args = append(args, outPath)

	t0 := time.Now()
	out, err := exec.Command("ffmpeg", args...).CombinedOutput()
	if err != nil {
		os.Remove(outPath)
		tail := string(out)
		if len(tail) > 1500 {
			tail = tail[len(tail)-1500:]
		}
		log.Printf("[ffmpeg] transcode %s -> %s FAILED after %v: %v: %s", src, outPath, time.Since(t0), err, tail)
		return fmt.Errorf("ffmpeg: %v", err)
	}
	log.Printf("[ffmpeg] transcode %s [%v..%v] -> %s OK after %v", filepath.Base(src), start, end, filepath.Base(outPath), time.Since(t0))
	return nil
}

// sanitizeFileName делает из названия трека безопасное имя файла, сохраняя
// любые письменности (кириллица, CJK и т.д.).
func sanitizeFileName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r == 0x7F:
			return -1
		case strings.ContainsRune(`/\:*?"<>|`, r):
			return '_'
		}
		return r
	}, s)
	s = strings.Trim(strings.TrimSpace(s), ".")
	const maxBytes = 150
	if len(s) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = strings.TrimSpace(s[:cut])
	}
	if s == "" {
		s = "track"
	}
	return s
}

// embeddedCue достаёт cue-sheet, вшитый в сам аудиофайл: тег CUESHEET
// (FLAC/Vorbis, APEv2 у APE/WavPack) или главы контейнера.
func embeddedCue(p *audioProbe) (*CueSheet, string) {
	if text := p.Tags["cuesheet"]; strings.TrimSpace(text) != "" {
		if sheet, err := parseCueSheet([]byte(text)); err == nil {
			return sheet, "встроенный в файл cue"
		}
	}
	if len(p.Chapters) >= 2 {
		sec := CueFileSection{}
		for i, ch := range p.Chapters {
			sec.Tracks = append(sec.Tracks, CueTrack{Number: i + 1, Title: ch.Title, Start: ch.Start, IsAudio: true})
		}
		sheet := &CueSheet{
			Performer: firstNonEmpty(p.Tags["album_artist"], p.Tags["artist"]),
			Title:     p.Tags["album"],
			Files:     []CueFileSection{sec},
			Encoding:  "UTF-8",
		}
		return sheet, "главы внутри файла"
	}
	return nil, ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// extractEmbeddedCover вынимает вложенную картинку (attached_pic) через
// ffmpeg — для форматов, которые не читает библиотека тегов (APE, WavPack,
// WAV, AIFF, DSF...).
func extractEmbeddedCover(path string) []byte {
	out, err := exec.Command("ffmpeg", "-hide_banner", "-nostdin", "-v", "error",
		"-i", path, "-an", "-map", "0:v:0", "-c:v", "copy", "-frames:v", "1", "-f", "image2pipe", "-").Output()
	if err != nil || len(out) < 100 {
		return nil
	}
	return out
}
