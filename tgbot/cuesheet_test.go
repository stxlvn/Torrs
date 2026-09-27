package tgbot

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	xunicode "golang.org/x/text/encoding/unicode"
)

func encodeCue(t *testing.T, enc encoding.Encoding, s string) []byte {
	t.Helper()
	b, err := enc.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return b
}

const jaCue = `REM GENRE "J-Pop"
REM DATE 2019
PERFORMER "宇多田ヒカル"
TITLE "初恋"
FILE "宇多田ヒカル - 初恋.wav" WAVE
  TRACK 01 AUDIO
    TITLE "Play A Love Song"
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    TITLE "あなた"
    INDEX 00 04:20:10
    INDEX 01 04:22:00
  TRACK 03 AUDIO
    TITLE "初恋"
    INDEX 01 09:10:00
  TRACK 04 AUDIO
    TITLE "誓い"
    INDEX 01 14:00:00
  TRACK 05 AUDIO
    TITLE "Forevermore"
    INDEX 01 18:30:00
`

const zhCue = `PERFORMER "周杰伦"
TITLE "叶惠美"
FILE "周杰伦 - 叶惠美.flac" WAVE
  TRACK 01 AUDIO
    TITLE "以父之名"
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    TITLE "懦夫"
    INDEX 01 05:42:00
  TRACK 03 AUDIO
    TITLE "晴天"
    INDEX 01 09:17:00
  TRACK 04 AUDIO
    TITLE "三年二班"
    INDEX 01 13:47:00
  TRACK 05 AUDIO
    TITLE "东风破"
    INDEX 01 18:28:00
`

const zhTwCue = `PERFORMER "鄧麗君"
TITLE "淡淡幽情"
FILE "鄧麗君 - 淡淡幽情.ape" WAVE
  TRACK 01 AUDIO
    TITLE "獨上西樓"
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    TITLE "但願人長久"
    INDEX 01 03:40:00
  TRACK 03 AUDIO
    TITLE "幾多愁"
    INDEX 01 07:50:00
  TRACK 04 AUDIO
    TITLE "芳草無情"
    INDEX 01 12:01:00
`

const koCue = `PERFORMER "아이유"
TITLE "꽃갈피"
FILE "아이유 - 꽃갈피.flac" WAVE
  TRACK 01 AUDIO
    TITLE "나의 옛날이야기"
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    TITLE "꽃"
    INDEX 01 04:30:00
  TRACK 03 AUDIO
    TITLE "삐에로는 우릴 보고 웃지"
    INDEX 01 08:15:00
`

const ruCue = `PERFORMER "Кино"
TITLE "Группа крови"
FILE "Кино - Группа крови.flac" WAVE
  TRACK 01 AUDIO
    TITLE "Группа крови"
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    TITLE "Закрой за мной дверь, я ухожу"
    INDEX 01 04:45:00
  TRACK 03 AUDIO
    TITLE "Война"
    INDEX 01 09:03:00
  TRACK 04 AUDIO
    TITLE "Спокойная ночь"
    INDEX 01 13:10:00
`

func checkDecoded(t *testing.T, name string, data []byte, hints []string, want, wantTitle string) {
	t.Helper()
	sheet, err := parseCueSheet(data, hints...)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if !strings.EqualFold(strings.Fields(sheet.Encoding)[0], want) {
		t.Errorf("%s: кодировка %q, ожидалась %q", name, sheet.Encoding, want)
	}
	if sheet.Files[0].Tracks[1].Title != wantTitle {
		t.Errorf("%s: второй трек %q, ожидался %q (кодировка %s)", name, sheet.Files[0].Tracks[1].Title, wantTitle, sheet.Encoding)
	}
}

func TestCueEncodings(t *testing.T) {
	cases := []struct {
		name, text string
		enc        encoding.Encoding
		want       string
		title      string
		hint       string
	}{
		{"ja-sjis", jaCue, japanese.ShiftJIS, "Shift_JIS", "あなた", "宇多田ヒカル - 初恋.flac"},
		{"ja-eucjp", jaCue, japanese.EUCJP, "EUC-JP", "あなた", "宇多田ヒカル - 初恋.flac"},
		{"zh-gbk", zhCue, simplifiedchinese.GBK, "GB18030", "懦夫", "周杰伦 - 叶惠美.flac"},
		{"zh-big5", zhTwCue, traditionalchinese.Big5, "Big5", "但願人長久", "鄧麗君 - 淡淡幽情.ape"},
		{"ko-euckr", koCue, korean.EUCKR, "EUC-KR", "꽃", "아이유 - 꽃갈피.flac"},
		{"ru-1251", ruCue, charmap.Windows1251, "windows-1251", "Закрой за мной дверь, я ухожу", "Кино - Группа крови.flac"},
		{"ru-koi8", ruCue, charmap.KOI8R, "KOI8-R", "Закрой за мной дверь, я ухожу", "Кино - Группа крови.flac"},
		{"ru-866", ruCue, charmap.CodePage866, "IBM866", "Закрой за мной дверь, я ухожу", "Кино - Группа крови.flac"},
	}
	for _, tc := range cases {
		data := encodeCue(t, tc.enc, tc.text)
		checkDecoded(t, tc.name+"+hint", data, []string{tc.hint}, tc.want, tc.title)
		checkDecoded(t, tc.name, data, nil, tc.want, tc.title)
	}
}

func TestCueUnicodeForms(t *testing.T) {
	bom := append([]byte{0xEF, 0xBB, 0xBF}, []byte(jaCue)...)
	checkDecoded(t, "utf8-bom", bom, nil, "UTF-8", "あなた")
	checkDecoded(t, "utf8", []byte(zhCue), nil, "UTF-8", "懦夫")

	le := encodeCue(t, xunicode.UTF16(xunicode.LittleEndian, xunicode.UseBOM), ruCue)
	checkDecoded(t, "utf16le-bom", le, nil, "UTF-16LE", "Закрой за мной дверь, я ухожу")
	leNoBOM := encodeCue(t, xunicode.UTF16(xunicode.LittleEndian, xunicode.IgnoreBOM), jaCue)
	checkDecoded(t, "utf16le", leNoBOM, nil, "UTF-16LE", "あなた")
	be := encodeCue(t, xunicode.UTF16(xunicode.BigEndian, xunicode.UseBOM), koCue)
	checkDecoded(t, "utf16be-bom", be, nil, "UTF-16BE", "꽃")
}

func TestCueMojibakeRepair(t *testing.T) {
	sjis := encodeCue(t, japanese.ShiftJIS, jaCue)
	var gb strings.Builder
	for _, b := range sjis {
		if r := charmap.Windows1252.DecodeByte(b); r != '\uFFFD' {
			gb.WriteRune(r)
		} else {
			gb.WriteRune(rune(b))
		}
	}
	garbled := []byte(gb.String())
	sheet, err := parseCueSheet(garbled)
	if err != nil {
		t.Fatal(err)
	}
	if got := sheet.Files[0].Tracks[1].Title; got != "あなた" {
		t.Errorf("кракозябры не исправлены: %q (%s)", got, sheet.Encoding)
	}

	latin := "PERFORMER \"Édith Piaf\"\nFILE \"a.flac\" WAVE\nTRACK 01 AUDIO\nTITLE \"Non, je ne regrette rien\"\nINDEX 01 00:00:00\nTRACK 02 AUDIO\nTITLE \"La Vie en rose\"\nINDEX 01 02:30:00\nTRACK 03 AUDIO\nTITLE \"Hymne à l'amour — Café Müller\"\nINDEX 01 05:00:00\n"
	sheet, err = parseCueSheet([]byte(latin))
	if err != nil {
		t.Fatal(err)
	}
	if sheet.Performer != "Édith Piaf" || !strings.Contains(sheet.Files[0].Tracks[2].Title, "Café Müller") {
		t.Errorf("нормальная латиница испорчена: %q %q (%s)", sheet.Performer, sheet.Files[0].Tracks[2].Title, sheet.Encoding)
	}
}

func TestCueMultiFileLP(t *testing.T) {
	cue := "REM DATE 1973\r\nPERFORMER \"Pink Floyd\"\r\nTITLE \"The Dark Side Of The Moon\"\r\n" +
		"FILE \"LP1.wav\" WAVE\r\n  TRACK 01 AUDIO\r\n    TITLE \"Speak to Me\"\r\n    INDEX 01 00:00:00\r\n" +
		"  TRACK 02 AUDIO\r\n    TITLE \"Breathe\"\r\n    INDEX 01 01:30:00\r\n" +
		"FILE \"LP2.wav\" WAVE\r\n  TRACK 03 AUDIO\r\n    TITLE \"Money\"\r\n    INDEX 01 00:00:00\r\n" +
		"  TRACK 04 AUDIO\r\n    TITLE \"Us and Them\"\r\n    INDEX 01 06:22:00\r\n" +
		"FILE \"LP3.wav\" WAVE\r\n  TRACK 05 AUDIO\r\n    TITLE \"Brain Damage\"\r\n    INDEX 01 00:00:00\r\n" +
		"  TRACK 06 AUDIO\r\n    TITLE \"Eclipse\"\r\n    INDEX 01 03:48:00\r\n"
	sheet, err := parseCueSheet([]byte(cue))
	if err != nil {
		t.Fatal(err)
	}
	if len(sheet.Files) != 3 || len(sheet.Files[1].Tracks) != 2 || sheet.Files[2].Tracks[1].Title != "Eclipse" {
		t.Fatalf("разбор LP: %+v", sheet.Files)
	}
	if sheet.PerTrackLayout() {
		t.Error("LP-образы ошибочно приняты за нарезанный релиз")
	}
	m := matchCueSections(sheet, []string{"Pink Floyd - LP1.flac", "Pink Floyd - LP2.flac", "Pink Floyd - LP3.flac"})
	if m[0] != "Pink Floyd - LP1.flac" || m[2] != "Pink Floyd - LP3.flac" {
		t.Errorf("сопоставление по порядку: %v", m)
	}
	m = matchCueSections(sheet, []string{"LP3.flac", "LP1.flac", "LP2.flac"})
	if m[0] != "LP1.flac" || m[1] != "LP2.flac" || m[2] != "LP3.flac" {
		t.Errorf("сопоставление по имени без расширения: %v", m)
	}
}

func TestCueNoncompliantGapsAppended(t *testing.T) {
	cue := `FILE "01 - One.wav" WAVE
  TRACK 01 AUDIO
    TITLE "One"
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    TITLE "Two"
    INDEX 00 03:58:20
FILE "02 - Two.wav" WAVE
    INDEX 01 00:00:00
  TRACK 03 AUDIO
    TITLE "Three"
    INDEX 00 04:10:00
FILE "03 - Three.wav" WAVE
    INDEX 01 00:00:00
`
	sheet, err := parseCueSheet([]byte(cue))
	if err != nil {
		t.Fatal(err)
	}
	if !sheet.PerTrackLayout() {
		t.Errorf("gaps appended не распознан как нарезанный релиз: %+v", sheet.Files)
	}
	if len(sheet.Files[1].Tracks) != 1 || sheet.Files[1].Tracks[0].Title != "Two" {
		t.Errorf("трек должен принадлежать файлу своего INDEX 01: %+v", sheet.Files)
	}
}

func TestCueQuirks(t *testing.T) {
	cue := "\uFEFFrem genre Rock\rperformer Artist Without Quotes\rtitle 'Single Quoted'\r" +
		"file C:\\Rips\\Album\\CDImage.wav WAVE\r" +
		"\ttrack 1 audio\r\t\ttitle \"Intro\"\r\t\tindex 00 00:00:00\r\t\tindex 01 00:32:00\r" +
		"\ttrack 2 MODE1/2352\r\t\tindex 01 03:00:00\r" +
		"\ttrack 3 audio\r\t\ttitle \"Say \"Hello\" Again\"\r\t\tpregap 00:02:00\r\t\tindex 01 05:00:00\r" +
		"\ttrack 4 audio\r\t\tindex 01 09:30:37\r"
	sheet, err := parseCueSheet([]byte(cue))
	if err != nil {
		t.Fatal(err)
	}
	if sheet.Performer != "Artist Without Quotes" || sheet.Title != "Single Quoted" || sheet.Genre != "Rock" {
		t.Errorf("шапка: %+v", sheet)
	}
	f := sheet.Files[0]
	if f.AudioFile != "C:\\Rips\\Album\\CDImage.wav" || len(f.Tracks) != 3 {
		t.Fatalf("секция: %+v", f)
	}
	if f.Tracks[1].Title != `Say "Hello" Again` {
		t.Errorf("кавычки внутри названия: %q", f.Tracks[1].Title)
	}
	if f.Tracks[2].Start != 9*time.Minute+30*time.Second+37*time.Second/75 {
		t.Errorf("кадры: %v", f.Tracks[2].Start)
	}
	cuts := sheet.Cuts(&f)
	if len(cuts) != 4 || !cuts[0].Hidden || cuts[0].End != 32*time.Second {
		t.Errorf("скрытый трек перед первым (HTOA): %+v", cuts)
	}
	if cuts[3].End != 0 || cuts[3].Title != "Track 04" || cuts[3].Performer != "Artist Without Quotes" {
		t.Errorf("последний трек: %+v", cuts[3])
	}
	if m := matchCueSections(sheet, []string{"Album.flac"}); m[0] != "Album.flac" {
		t.Errorf("единственный файл при несовпадении имени: %v", m)
	}
	if m := matchCueSections(sheet, []string{"Album.flac", "Bonus.flac"}); len(m) != 0 {
		t.Errorf("не должен угадывать при нескольких кандидатах: %v", m)
	}
}

func TestCueShortStrings(t *testing.T) {
	type c struct {
		enc  encoding.Encoding
		name string
		s    string
	}
	cases := []c{
		{japanese.ShiftJIS, "Shift_JIS", "坂本龍一"},
		{japanese.ShiftJIS, "Shift_JIS", "東京事変 - 教育"},
		{japanese.ShiftJIS, "Shift_JIS", "千と千尋の神隠し サウンドトラック"},
		{japanese.ShiftJIS, "Shift_JIS", "ｱﾆﾒ 主題歌集"},
		{japanese.EUCJP, "EUC-JP", "久石譲 風の谷のナウシカ"},
		{simplifiedchinese.GBK, "GB18030", "王菲"},
		{simplifiedchinese.GBK, "GB18030", "陈奕迅 - 十年"},
		{simplifiedchinese.GBK, "GB18030", "中国民族音乐精选 第一集"},
		{traditionalchinese.Big5, "Big5", "張學友 - 吻別"},
		{traditionalchinese.Big5, "Big5", "五月天 知足"},
		{korean.EUCKR, "EUC-KR", "방탄소년단"},
		{charmap.Windows1251, "windows-1251", "Ария"},
		{charmap.Windows1251, "windows-1251", "Сплин - Гранатовый альбом"},
		{charmap.KOI8R, "KOI8-R", "Сплин - Гранатовый альбом"},
		{charmap.CodePage866, "IBM866", "ДДТ - Осень"},
		{charmap.Windows1252, "windows-1252", "Sigur Rós - Ágætis byrjun"},
		{charmap.Windows1250, "windows-1250", "Czesław Niemen - Dziwny jest ten świat"},
		{charmap.Windows1253, "windows-1253", "Μίκης Θεοδωράκης"},
	}
	for _, tc := range cases {
		cue := fmt.Sprintf("PERFORMER \"%s\"\nTITLE \"%s\"\nFILE \"CDImage.wav\" WAVE\n TRACK 01 AUDIO\n  TITLE \"One\"\n  INDEX 01 00:00:00\n TRACK 02 AUDIO\n  TITLE \"Two\"\n  INDEX 01 03:00:00\n", tc.s, tc.s)
		data := encodeCue(t, tc.enc, cue)
		sh, err := parseCueSheet(data)
		if err != nil {
			t.Fatal(err)
		}
		sh2, _ := parseCueSheet(data, tc.s+" - Greatest Hits (2001) [FLAC]", "CDImage.flac")
		if sh2.Performer != tc.s {
			t.Errorf("с подсказкой папки: %s -> %s %q", tc.name, sh2.Encoding, sh2.Performer)
		}
		if sh.Performer != tc.s {
			t.Errorf("без подсказок: %s -> %s %q", tc.name, sh.Encoding, sh.Performer)
		}
	}
}
