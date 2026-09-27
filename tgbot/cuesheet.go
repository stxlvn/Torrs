package tgbot

import (
	"bytes"
	"fmt"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gogs/chardet"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	xunicode "golang.org/x/text/encoding/unicode"
	"golang.org/x/text/encoding/unicode/utf32"
	"golang.org/x/text/unicode/norm"
)

type CueTrack struct {
	Number     int
	Title      string
	Performer  string
	Songwriter string
	ISRC       string
	Start      time.Duration
	IsAudio    bool

	indexes   map[int]time.Duration
	indexFile map[int]int
}

type CueFileSection struct {
	AudioFile string
	FileType  string
	Tracks    []CueTrack
}

type CueSheet struct {
	Performer  string
	Title      string
	Songwriter string
	Genre      string
	Date       string
	DiscNumber string
	TotalDiscs string
	Composer   string
	Comment    string
	Files      []CueFileSection
	Encoding   string
}

type CueCut struct {
	Number     int
	Title      string
	Performer  string
	Songwriter string
	ISRC       string
	Start      time.Duration
	End        time.Duration
	Hidden     bool
}

const htoaMinDuration = 10 * time.Second

func (s *CueSheet) AudioTrackCount() int {
	n := 0
	for _, f := range s.Files {
		n += len(f.Tracks)
	}
	return n
}

// PerTrackLayout — cue описывает уже нарезанный релиз: каждый FILE содержит
// не больше одного трека.
func (s *CueSheet) PerTrackLayout() bool {
	withTracks := 0
	for _, f := range s.Files {
		if len(f.Tracks) > 1 {
			return false
		}
		if len(f.Tracks) == 1 {
			withTracks++
		}
	}
	return withTracks > 1
}

func (s *CueSheet) Cuts(sec *CueFileSection) []CueCut {
	var cuts []CueCut
	for i, tr := range sec.Tracks {
		var end time.Duration
		if i+1 < len(sec.Tracks) {
			end = sec.Tracks[i+1].Start
		}
		performer := tr.Performer
		if performer == "" {
			performer = s.Performer
		}
		title := strings.TrimSpace(tr.Title)
		if title == "" {
			title = fmt.Sprintf("Track %02d", tr.Number)
		}
		songwriter := tr.Songwriter
		if songwriter == "" {
			songwriter = s.Songwriter
		}
		cuts = append(cuts, CueCut{
			Number: tr.Number, Title: title, Performer: performer,
			Songwriter: songwriter, ISRC: tr.ISRC, Start: tr.Start, End: end,
		})
	}
	if len(cuts) > 0 && cuts[0].Start >= htoaMinDuration {
		hidden := CueCut{Number: 0, Title: "Hidden Track", Performer: cuts[0].Performer, End: cuts[0].Start, Hidden: true}
		cuts = append([]CueCut{hidden}, cuts...)
	}
	return cuts
}

// ---------------------------------------------------------------- разбор

func parseCueTime(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	bad := fmt.Errorf("некорректный формат времени %q", s)
	switch len(parts) {
	case 3:
		mm, err1 := strconv.Atoi(parts[0])
		ss, err2 := strconv.Atoi(parts[1])
		ff, err3 := strconv.ParseFloat(parts[2], 64)
		if err1 != nil || err2 != nil || err3 != nil || mm < 0 || ss < 0 || ff < 0 {
			return 0, bad
		}
		return time.Duration(mm)*time.Minute + time.Duration(ss)*time.Second + time.Duration(ff*float64(time.Second)/75), nil
	case 2:
		mm, err1 := strconv.Atoi(parts[0])
		ss, err2 := strconv.ParseFloat(parts[1], 64)
		if err1 != nil || err2 != nil || mm < 0 || ss < 0 {
			return 0, bad
		}
		return time.Duration(mm)*time.Minute + time.Duration(ss*float64(time.Second)), nil
	}
	return 0, bad
}

func parseCueQuoted(rest string) string {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return ""
	}
	q := rest[0]
	if q == '"' || q == '\'' {
		if last := strings.LastIndexByte(rest, q); last > 0 {
			return strings.TrimSpace(rest[1:last])
		}
		return strings.TrimSpace(rest[1:])
	}
	return rest
}

var cueFileTypes = map[string]bool{
	"WAVE": true, "WAV": true, "MP3": true, "AIFF": true, "BINARY": true, "MOTOROLA": true,
	"FLAC": true, "APE": true, "WV": true, "WAVPACK": true, "OGG": true, "M4A": true, "AAC": true,
}

func parseCueFileName(rest string) (name, ftype string) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", ""
	}
	if q := rest[0]; q == '"' || q == '\'' {
		if last := strings.LastIndexByte(rest, q); last > 0 {
			return rest[1:last], strings.ToUpper(strings.TrimSpace(rest[last+1:]))
		}
		return strings.TrimSpace(rest[1:]), ""
	}
	fields := strings.Fields(rest)
	if len(fields) > 1 && cueFileTypes[strings.ToUpper(fields[len(fields)-1])] {
		idx := strings.LastIndex(rest, fields[len(fields)-1])
		return strings.TrimSpace(rest[:idx]), strings.ToUpper(fields[len(fields)-1])
	}
	return rest, ""
}

func splitCueLines(text string) []string {
	text = strings.TrimPrefix(text, "\uFEFF")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.Split(text, "\n")
}

func cueKeyword(line string) (kw, rest string) {
	line = strings.TrimSpace(strings.TrimPrefix(line, "\uFEFF"))
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return strings.ToUpper(line), ""
	}
	return strings.ToUpper(line[:i]), strings.TrimSpace(line[i+1:])
}

func parseCueSheetText(text string) (*CueSheet, error) {
	sheet := &CueSheet{}
	type rawFile struct {
		name, ftype string
	}
	var files []rawFile
	curFile := -1
	var tracks []*CueTrack
	var cur *CueTrack

	for _, line := range splitCueLines(text) {
		kw, rest := cueKeyword(line)
		if kw == "" {
			continue
		}
		switch kw {
		case "FILE":
			name, ft := parseCueFileName(rest)
			files = append(files, rawFile{name: name, ftype: ft})
			curFile = len(files) - 1
		case "TRACK":
			fields := strings.Fields(rest)
			if len(fields) == 0 {
				continue
			}
			num, err := strconv.Atoi(fields[0])
			if err != nil {
				continue
			}
			isAudio := len(fields) < 2 || strings.EqualFold(fields[1], "AUDIO")
			cur = &CueTrack{Number: num, IsAudio: isAudio, indexes: map[int]time.Duration{}, indexFile: map[int]int{}}
			tracks = append(tracks, cur)
		case "INDEX":
			fields := strings.Fields(rest)
			if cur == nil || len(fields) < 2 {
				continue
			}
			n, err := strconv.Atoi(fields[0])
			if err != nil {
				continue
			}
			t, err := parseCueTime(fields[1])
			if err != nil {
				continue
			}
			if curFile < 0 {
				files = append(files, rawFile{})
				curFile = 0
			}
			cur.indexes[n] = t
			cur.indexFile[n] = curFile
		case "TITLE":
			if cur != nil {
				cur.Title = parseCueQuoted(rest)
			} else {
				sheet.Title = parseCueQuoted(rest)
			}
		case "PERFORMER":
			if cur != nil {
				cur.Performer = parseCueQuoted(rest)
			} else {
				sheet.Performer = parseCueQuoted(rest)
			}
		case "SONGWRITER":
			if cur != nil {
				cur.Songwriter = parseCueQuoted(rest)
			} else {
				sheet.Songwriter = parseCueQuoted(rest)
			}
		case "ISRC":
			if cur != nil {
				cur.ISRC = parseCueQuoted(rest)
			}
		case "REM":
			if cur != nil {
				continue
			}
			sub, val := cueKeyword(rest)
			val = parseCueQuoted(val)
			switch sub {
			case "GENRE":
				sheet.Genre = val
			case "DATE", "YEAR":
				sheet.Date = val
			case "DISCNUMBER":
				sheet.DiscNumber = val
			case "TOTALDISCS":
				sheet.TotalDiscs = val
			case "COMPOSER":
				sheet.Composer = val
			case "COMMENT":
				sheet.Comment = val
			}
		}
	}

	sheet.Files = make([]CueFileSection, len(files))
	for i, f := range files {
		sheet.Files[i] = CueFileSection{AudioFile: f.name, FileType: f.ftype}
	}
	for _, tr := range tracks {
		if !tr.IsAudio {
			continue
		}
		idx := -1
		if _, ok := tr.indexes[1]; ok {
			idx = 1
		} else {
			for n := range tr.indexes {
				if n >= 1 && (idx < 0 || n < idx) {
					idx = n
				}
			}
			if idx < 0 {
				if _, ok := tr.indexes[0]; ok {
					idx = 0
				}
			}
		}
		if idx < 0 {
			continue
		}
		tr.Start = tr.indexes[idx]
		fi := tr.indexFile[idx]
		sheet.Files[fi].Tracks = append(sheet.Files[fi].Tracks, *tr)
	}
	for i := range sheet.Files {
		t := sheet.Files[i].Tracks
		sort.SliceStable(t, func(a, b int) bool { return t[a].Start < t[b].Start })
		dedup := t[:0]
		for j, tr := range t {
			if j > 0 && tr.Start == dedup[len(dedup)-1].Start {
				continue
			}
			dedup = append(dedup, tr)
		}
		sheet.Files[i].Tracks = dedup
	}
	if sheet.AudioTrackCount() == 0 {
		return nil, fmt.Errorf("cue-sheet не содержит аудиотреков")
	}
	return sheet, nil
}

// parseCueSheet разбирает cue в любой кодировке; hintNames — реальные имена
// аудиофайлов рядом с cue, по ним выбирается кодировка FILE-строк.
func parseCueSheet(data []byte, hintNames ...string) (*CueSheet, error) {
	text, enc := decodeCueBytes(data, hintNames)
	sheet, err := parseCueSheetText(text)
	if err != nil {
		return nil, err
	}
	sheet.Encoding = enc
	return sheet, nil
}

// ------------------------------------------------------- сопоставление FILE

func normCueName(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "./")
	return strings.ToLower(norm.NFC.String(strings.TrimSpace(p)))
}

func cueStem(p string) string {
	b := path.Base(p)
	if ext := path.Ext(b); ext != "" && len(ext) <= 6 {
		return strings.TrimSuffix(b, ext)
	}
	return b
}

func looseCueName(p string) string {
	s := norm.NFKC.String(strings.ToLower(cueStem(normCueName(p))))
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// matchCueSections сопоставляет FILE-секции (с треками) реальным файлам.
// rels — пути аудиофайлов относительно папки cue ("/"-разделитель).
// Возвращает индекс секции -> элемент rels.
func matchCueSections(sheet *CueSheet, rels []string) map[int]string {
	result := map[int]string{}
	usedRel := map[string]bool{}
	var secs []int
	for i, f := range sheet.Files {
		if len(f.Tracks) > 0 {
			secs = append(secs, i)
		}
	}
	keys := []func(string) string{
		normCueName,
		func(p string) string { return path.Base(normCueName(p)) },
		func(p string) string { return cueStem(normCueName(p)) },
		looseCueName,
	}
	for _, key := range keys {
		for _, si := range secs {
			if _, done := result[si]; done {
				continue
			}
			want := key(sheet.Files[si].AudioFile)
			if want == "" {
				continue
			}
			var found []string
			for _, r := range rels {
				if !usedRel[r] && key(r) == want {
					found = append(found, r)
				}
			}
			if len(found) == 1 {
				result[si] = found[0]
				usedRel[found[0]] = true
			}
		}
	}
	var restSecs []int
	for _, si := range secs {
		if _, ok := result[si]; !ok {
			restSecs = append(restSecs, si)
		}
	}
	var restRels []string
	for _, r := range rels {
		if !usedRel[r] {
			restRels = append(restRels, r)
		}
	}
	if len(restSecs) > 0 && len(restSecs) == len(restRels) {
		sort.SliceStable(restRels, func(a, b int) bool { return naturalLess(restRels[a], restRels[b]) })
		for i, si := range restSecs {
			result[si] = restRels[i]
		}
	}
	return result
}

// --------------------------------------------------------------- кодировки

type cueCharset struct {
	name   string
	enc    encoding.Encoding
	ja     bool
	family string
}

var cueLegacyCharsets = []cueCharset{
	{"windows-1251", charmap.Windows1251, false, ""},
	{"KOI8-R", charmap.KOI8R, false, ""},
	{"IBM866", charmap.CodePage866, false, ""},
	{"KOI8-U", charmap.KOI8U, false, ""},
	{"ISO-8859-5", charmap.ISO8859_5, false, ""},
	{"windows-1252", charmap.Windows1252, false, ""},
	{"windows-1250", charmap.Windows1250, false, ""},
	{"windows-1253", charmap.Windows1253, false, ""},
	{"windows-1254", charmap.Windows1254, false, ""},
	{"windows-1257", charmap.Windows1257, false, ""},
	{"Shift_JIS", japanese.ShiftJIS, true, "jis"},
	{"EUC-JP", japanese.EUCJP, true, "jis"},
	{"GB18030", simplifiedchinese.GB18030, false, "gb"},
	{"Big5", traditionalchinese.Big5, false, "big5"},
	{"EUC-KR", korean.EUCKR, false, "kr"},
}

var chardetAlias = map[string]string{
	"ISO-8859-1": "windows-1252",
	"ISO-8859-2": "windows-1250",
	"ISO-8859-7": "windows-1253",
	"ISO-8859-9": "windows-1254",
}

func decodeWith(enc encoding.Encoding, data []byte) (string, bool) {
	out, err := enc.NewDecoder().Bytes(data)
	if err != nil {
		return "", false
	}
	s := string(out)
	return s, !strings.ContainsRune(s, utf8.RuneError)
}

func detectBOM(data []byte) (encoding.Encoding, string, int) {
	switch {
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE, 0x00, 0x00}):
		return utf32.UTF32(utf32.LittleEndian, utf32.IgnoreBOM), "UTF-32LE", 4
	case bytes.HasPrefix(data, []byte{0x00, 0x00, 0xFE, 0xFF}):
		return utf32.UTF32(utf32.BigEndian, utf32.IgnoreBOM), "UTF-32BE", 4
	case bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}):
		return xunicode.UTF8, "UTF-8", 3
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}):
		return xunicode.UTF16(xunicode.LittleEndian, xunicode.IgnoreBOM), "UTF-16LE", 2
	case bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		return xunicode.UTF16(xunicode.BigEndian, xunicode.IgnoreBOM), "UTF-16BE", 2
	}
	return nil, "", 0
}

// guessUTF16 распознаёт UTF-16 без BOM по нулевым байтам ASCII-символов.
func guessUTF16(data []byte) (encoding.Encoding, string) {
	if len(data) < 8 {
		return nil, ""
	}
	var evenZero, oddZero int
	for i, b := range data {
		if b == 0 {
			if i%2 == 0 {
				evenZero++
			} else {
				oddZero++
			}
		}
	}
	half := len(data) / 2
	switch {
	case oddZero > half*3/10 && evenZero < half/20:
		return xunicode.UTF16(xunicode.LittleEndian, xunicode.IgnoreBOM), "UTF-16LE"
	case evenZero > half*3/10 && oddZero < half/20:
		return xunicode.UTF16(xunicode.BigEndian, xunicode.IgnoreBOM), "UTF-16BE"
	}
	return nil, ""
}

func isASCII(data []byte) bool {
	for _, b := range data {
		if b >= 0x80 {
			return false
		}
	}
	return true
}

// decodeCueBytes приводит cue к UTF-8 и возвращает название кодировки.
func decodeCueBytes(data []byte, hintNames []string) (string, string) {
	if enc, name, n := detectBOM(data); enc != nil {
		if s, ok := decodeWith(enc, data[n:]); ok {
			return s, name
		}
	}
	if enc, name := guessUTF16(data); enc != nil {
		if s, ok := decodeWith(enc, data); ok {
			return s, name
		}
	}
	if isASCII(data) {
		return string(data), "ASCII"
	}
	if utf8.Valid(data) {
		s := string(data)
		if fixed, enc, ok := repairMojibake(s, hintNames); ok {
			return fixed, enc
		}
		return s, "UTF-8"
	}
	return detectLegacy(data, hintNames)
}

func detectLegacy(data []byte, hintNames []string) (string, string) {
	conf := map[string]int{}
	if results, err := chardet.NewTextDetector().DetectAll(data); err == nil {
		for _, r := range results {
			name := r.Charset
			if a, ok := chardetAlias[name]; ok {
				name = a
			}
			if r.Confidence > conf[name] {
				conf[name] = r.Confidence
			}
		}
	}

	bestScore := math.Inf(-1)
	var best, bestName string
	for _, cs := range cueLegacyCharsets {
		s, ok := decodeWith(cs.enc, data)
		if !ok {
			continue
		}
		score := float64(fileNameMatches(s, hintNames))*1000 + hintOverlap(s, hintNames)*150 +
			float64(conf[cs.name]) + textPlausibility(s, cs.family)/2
		if cs.family == "kr" {
			hangul, han := 0, 0
			for _, r := range s {
				if r >= 0x3130 && r <= 0x318F {
					han++
				} else if unicode.Is(unicode.Hangul, r) {
					hangul++
				} else if unicode.Is(unicode.Han, r) {
					han++
				}
			}
			if hangul > 0 && han == 0 {
				score += 15
			}
		}
		if cs.ja {
			if countKana(s) > 0 {
				score += 10
			} else {
				score -= 15
			}
		}
		if score > bestScore {
			bestScore, best, bestName = score, s, cs.name
		}
	}
	if bestName == "" {
		s, _ := decodeWith(charmap.Windows1251, data)
		return strings.ToValidUTF8(s, "?"), "windows-1251?"
	}
	return best, bestName
}

// repairMojibake чинит UTF-8, в который когда-то прочитали байты чужой
// кодировки как windows-1252/latin-1 ("Ïðèâåò", "‚±‚ñ‚É‚¿‚Í").
func repairMojibake(s string, hintNames []string) (string, string, bool) {
	var suspicious int
	for _, r := range s {
		switch {
		case r < 0x80:
		case r <= 0xFF:
			suspicious++
		case isCP1252Special(r):
			suspicious++
		default:
			return "", "", false
		}
	}
	if suspicious < 4 {
		return "", "", false
	}
	raw := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r < 0x100:
			raw = append(raw, byte(r))
		default:
			b, ok := charmap.Windows1252.EncodeRune(r)
			if !ok {
				return "", "", false
			}
			raw = append(raw, b)
		}
	}
	var fixed, enc string
	if utf8.Valid(raw) {
		fixed, enc = string(raw), "UTF-8 (исправлены кракозябры)"
	} else {
		fixed, enc = detectLegacy(raw, hintNames)
		enc += " (исправлены кракозябры)"
	}
	if dominantScript(fixed) == "latin" || textPlausibility(fixed, "") < textPlausibility(s, "")+20 {
		return "", "", false
	}
	return fixed, enc, true
}

func isCP1252Special(r rune) bool {
	return strings.ContainsRune("€‚ƒ„…†‡ˆ‰Š‹ŒŽ‘’“”•–—˜™š›œžŸ", r)
}

func fileNameMatches(text string, hintNames []string) int {
	if len(hintNames) == 0 {
		return 0
	}
	hints := map[string]bool{}
	for _, h := range hintNames {
		hints[cueStem(path.Base(normCueName(h)))] = true
	}
	n := 0
	for _, line := range splitCueLines(text) {
		kw, rest := cueKeyword(line)
		if kw != "FILE" {
			continue
		}
		name, _ := parseCueFileName(rest)
		if isASCII([]byte(name)) {
			continue
		}
		if hints[cueStem(path.Base(normCueName(name)))] {
			n++
		}
	}
	return n
}

// hintOverlap — доля не-ASCII символов текста, встречающихся в именах
// папок/файлов раздачи (они уже в UTF-8 и обычно содержат исполнителя и
// альбом).
func hintOverlap(text string, hints []string) float64 {
	set := map[rune]bool{}
	for _, h := range hints {
		for _, r := range norm.NFC.String(h) {
			if r >= 0x80 {
				set[r] = true
			}
		}
	}
	if len(set) == 0 {
		return 0
	}
	var total, hit float64
	for _, r := range text {
		if r >= 0x80 {
			total++
			if set[r] {
				hit++
			}
		}
	}
	if total == 0 {
		return 0
	}
	return hit / total
}

// hanWeight — насколько иероглиф типичен для текста в кодировке данного
// семейства: знаки первого уровня GB2312/Big5/JIS — обычный текст, второго
// — реже, вне стандартных таблиц — почти всегда признак неверной кодировки.
func hanWeight(r rune, family string) float64 {
	var enc encoding.Encoding
	switch family {
	case "gb":
		enc = simplifiedchinese.GBK
	case "big5":
		enc = traditionalchinese.Big5
	case "jis":
		enc = japanese.ShiftJIS
	case "kr":
		enc = korean.EUCKR
	default:
		return 1
	}
	b, err := enc.NewEncoder().Bytes([]byte(string(r)))
	if err != nil || len(b) != 2 {
		return -0.5
	}
	code := int(b[0])<<8 | int(b[1])
	switch family {
	case "gb":
		if b[1] >= 0xA1 && b[0] >= 0xB0 && b[0] <= 0xD7 {
			return 1
		}
		if b[1] >= 0xA1 && b[0] >= 0xD8 && b[0] <= 0xF7 {
			return 0.6
		}
	case "big5":
		if code >= 0xA440 && code <= 0xC67E {
			return 1
		}
		if code >= 0xC940 && code <= 0xF9D5 {
			return 0.6
		}
	case "jis":
		if code >= 0x889F && code <= 0x9872 {
			return 1
		}
		if code >= 0x989F && code <= 0xEAA4 {
			return 0.6
		}
	case "kr":
		return 0.4
	}
	return -0.5
}

func countKana(s string) int {
	n := 0
	for _, r := range s {
		if unicode.In(r, unicode.Hiragana, unicode.Katakana) {
			n++
		}
	}
	return n
}

func runeScript(r rune) string {
	switch {
	case unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Han):
		return "cjk"
	case unicode.Is(unicode.Hangul, r):
		return "hangul"
	case unicode.Is(unicode.Cyrillic, r):
		return "cyrillic"
	case unicode.Is(unicode.Greek, r):
		return "greek"
	case unicode.In(r, unicode.Hebrew, unicode.Arabic, unicode.Thai):
		return "other"
	case unicode.Is(unicode.Latin, r):
		return "latin"
	}
	return ""
}

func dominantScript(s string) string {
	counts := map[string]int{}
	for _, r := range s {
		if r >= 0x80 {
			if sc := runeScript(r); sc != "" {
				counts[sc]++
			}
		}
	}
	best, bestN := "", 0
	for sc, n := range counts {
		if n > bestN {
			best, bestN = sc, n
		}
	}
	return best
}

// textPlausibility оценивает, насколько не-ASCII часть текста похожа на
// осмысленный текст (а не на кракозябры): ~100 — связный текст одного
// письма, <0 — мусор.
func textPlausibility(s string, family string) float64 {
	var nonASCII, good, bad float64
	scripts := map[string]float64{}
	for _, r := range s {
		if r < 0x80 {
			continue
		}
		nonASCII++
		switch {
		case r == utf8.RuneError:
			bad += 5
		case r < 0xA0:
			bad += 3
		case r >= 0xFF61 && r <= 0xFF9F:
			if family == "jis" {
				good += 0.3
				scripts["cjk"]++
			} else {
				bad += 2
			}
		case unicode.Is(unicode.Han, r) && r < 0x20000:
			w := hanWeight(r, family)
			if w > 0 {
				good += w
				scripts["cjk"]++
			} else {
				bad -= w * 2
			}
		case r >= 0x2500 && r <= 0x259F, r >= 0x3130 && r <= 0x318F:
			bad += 2
		case r >= 0xE000 && r <= 0xF8FF:
			bad += 3
		case r >= 0x3400 && r <= 0x4DBF, r >= 0xF900 && r <= 0xFAFF, r >= 0x20000:
			bad += 1
		case r >= 0x2010 && r <= 0x206F, r >= 0x3000 && r <= 0x303F, r >= 0xFF01 && r <= 0xFF5E, r == 0xA0, r == 0xAB, r == 0xBB, r == 0xB7, r == 0x2116:
			good += 0.5
		default:
			if sc := runeScript(r); sc != "" && unicode.IsLetter(r) {
				scripts[sc]++
				if sc == "latin" {
					good += 0.5
				} else {
					good++
				}
			} else {
				bad++
			}
		}
	}
	if nonASCII == 0 {
		return 50
	}
	var total, max float64
	for _, n := range scripts {
		total += n
		if n > max {
			max = n
		}
	}
	coherence := 1.0
	if total > 0 {
		coherence = max / total
	}
	score := (good*coherence - bad) / nonASCII * 100

	words, odd := 0, 0
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) }) {
		rs := []rune(w)
		if len(rs) < 2 {
			continue
		}
		hasHigh := false
		for _, r := range rs {
			if r >= 0x80 {
				hasHigh = true
				break
			}
		}
		if !hasHigh {
			continue
		}
		words++
		hasASCIILetter, hasHanOrHangul := false, false
		for _, r := range rs {
			if r < 0x80 {
				hasASCIILetter = true
			} else if unicode.In(r, unicode.Han, unicode.Hangul) {
				hasHanOrHangul = true
			}
		}
		if hasASCIILetter && hasHanOrHangul {
			odd++
			continue
		}
		switch dominantScript(w) {
		case "cyrillic":
			for i := 1; i < len(rs); i++ {
				if unicode.IsLower(rs[i-1]) && unicode.IsUpper(rs[i]) {
					odd++
					break
				}
			}
		case "latin":
			if len(rs) >= 2 {
				allHigh := true
				for _, r := range rs {
					if r < 0x80 {
						allHigh = false
						break
					}
				}
				if allHigh {
					odd++
				}
			}
		}
	}
	if words > 0 {
		score -= 60 * float64(odd) / float64(words)
	}
	return score
}
