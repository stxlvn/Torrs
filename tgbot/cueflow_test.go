package tgbot

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/text/encoding/japanese"
)

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindCueForFile(t *testing.T) {
	needFFmpeg(t)
	root := t.TempDir()
	src := filepath.Join(root, "src.flac")
	makeSource(t, src, "-c:a", "flac")
	audio, _ := os.ReadFile(src)
	os.Remove(src)
	fc := func(p string) cueFileCtx { return cueFileCtx{AudioPath: p, Hash: "0000", RootTmp: root} }

	lpDir := filepath.Join(root, "Pink Floyd - 1973 - DSOTM (Vinyl)")
	for _, n := range []string{"Side A.flac", "Side B.flac", "Side C.flac"} {
		writeFile(t, filepath.Join(lpDir, n), audio)
	}
	lpCue := "PERFORMER \"Pink Floyd\"\nTITLE \"DSOTM\"\n" +
		"FILE \"LP1.wav\" WAVE\n TRACK 01 AUDIO\n  TITLE \"A1\"\n  INDEX 01 00:00:00\n TRACK 02 AUDIO\n  TITLE \"A2\"\n  INDEX 01 00:10:00\n" +
		"FILE \"LP2.wav\" WAVE\n TRACK 03 AUDIO\n  TITLE \"B1\"\n  INDEX 01 00:00:00\n TRACK 04 AUDIO\n  TITLE \"B2\"\n  INDEX 01 00:10:00\n" +
		"FILE \"LP3.wav\" WAVE\n TRACK 05 AUDIO\n  TITLE \"C1\"\n  INDEX 01 00:00:00\n TRACK 06 AUDIO\n  TITLE \"C2\"\n  INDEX 01 00:10:00\n"
	writeFile(t, filepath.Join(lpDir, "Pink Floyd - DSOTM.cue"), []byte(lpCue))
	m, _ := findCueForFile(fc(filepath.Join(lpDir, "Side B.flac")))
	if m == nil || len(m.qualifyingSections()) != 3 || m.Sheet.Files[m.Section].Tracks[0].Title != "B1" {
		t.Fatalf("LP-группа: %+v", m)
	}

	splitDir := filepath.Join(root, "Already Split")
	cue := "FILE \"01 - One.flac\" WAVE\n TRACK 01 AUDIO\n  TITLE \"One\"\n  PERFORMER \"X\"\n  INDEX 01 00:00:00\n" +
		"FILE \"02 - Two.flac\" WAVE\n TRACK 02 AUDIO\n  TITLE \"Two\"\n  INDEX 01 00:00:00\n"
	writeFile(t, filepath.Join(splitDir, "album.cue"), []byte(cue))
	writeFile(t, filepath.Join(splitDir, "01 - One.flac"), audio)
	writeFile(t, filepath.Join(splitDir, "02 - Two.flac"), audio)
	m, meta := findCueForFile(fc(filepath.Join(splitDir, "01 - One.flac")))
	if m != nil || meta == nil || meta.Title != "One" || meta.Artist != "X" {
		t.Fatalf("нарезанный релиз: %+v %+v", m, meta)
	}

	boxDir := filepath.Join(root, "坂本龍一 - Box")
	jaCueText := "PERFORMER \"坂本龍一\"\nTITLE \"Box\"\n" +
		"FILE \"CD1\\坂本龍一 - Disc 1.wav\" WAVE\n TRACK 01 AUDIO\n  TITLE \"戦場のメリークリスマス\"\n  INDEX 01 00:00:00\n TRACK 02 AUDIO\n  TITLE \"ラストエンペラー\"\n  INDEX 01 00:12:00\n" +
		"FILE \"CD2\\坂本龍一 - Disc 2.wav\" WAVE\n TRACK 03 AUDIO\n  TITLE \"エナジー・フロー\"\n  INDEX 01 00:00:00\n TRACK 04 AUDIO\n  TITLE \"千のナイフ\"\n  INDEX 01 00:12:00\n"
	sj, _ := japanese.ShiftJIS.NewEncoder().Bytes([]byte(jaCueText))
	writeFile(t, filepath.Join(boxDir, "box.cue"), sj)
	writeFile(t, filepath.Join(boxDir, "CD1", "坂本龍一 - Disc 1.flac"), audio)
	writeFile(t, filepath.Join(boxDir, "CD2", "坂本龍一 - Disc 2.flac"), audio)
	m, _ = findCueForFile(fc(filepath.Join(boxDir, "CD2", "坂本龍一 - Disc 2.flac")))
	if m == nil || m.Sheet.Encoding != "Shift_JIS" || m.Sheet.Files[m.Section].Tracks[1].Title != "千のナイフ" {
		t.Fatalf("cue в родительской папке (Shift_JIS, пути CD1\\): %+v", m)
	}

	imgDir := filepath.Join(root, "Image")
	writeFile(t, filepath.Join(imgDir, "Artist - Album.ape"), audio)
	writeFile(t, filepath.Join(imgDir, "Artist - Album.cue"), []byte("FILE \"CDImage.wav\" WAVE\n TRACK 01 AUDIO\n  INDEX 01 00:00:00\n TRACK 02 AUDIO\n  INDEX 01 00:05:00\n TRACK 03 AUDIO\n  INDEX 01 00:20:00\n"))
	m, _ = findCueForFile(fc(filepath.Join(imgDir, "Artist - Album.ape")))
	if m == nil || len(m.Sheet.Files[m.Section].Tracks) != 3 {
		t.Fatalf("CDImage.wav против .ape: %+v", m)
	}
	writeFile(t, filepath.Join(imgDir, userCueName), []byte("FILE \"x.wav\" WAVE\n TRACK 01 AUDIO\n  TITLE \"Mine\"\n  INDEX 01 00:00:00\n TRACK 02 AUDIO\n  INDEX 01 00:15:00\n"))
	m, _ = findCueForFile(fc(filepath.Join(imgDir, "Artist - Album.ape")))
	if m == nil || m.Sheet.Files[m.Section].Tracks[0].Title != "Mine" || m.Source != "ваш cue, кодировка ASCII" {
		t.Fatalf("свой cue должен перекрывать cue раздачи: %+v", m)
	}

	bare := filepath.Join(root, "Bare", "Mix.flac")
	writeFile(t, bare, audio)
	if m, meta := findCueForFile(fc(bare)); m != nil || meta != nil {
		t.Fatalf("файл без cue: %+v %+v", m, meta)
	}
}
