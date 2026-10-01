package torr

import (
	"testing"

	"torrsru/tgbot/torr/state"
)

func TestFilePhase(t *testing.T) {
	cases := map[string]int{
		"Album/Scans/Front.JPG": phaseImages,
		"Album/cover.png":       phaseImages,
		"Album/01 - Track.flac": phaseAudio,
		"Album/Side A.dsf":      phaseAudio,
		"Album/Disc.iso":        phaseAudio,
		"Album/Album.cue":       phaseCue,
		"Album/rip.log":         phaseOther,
		"Album/foo_dr.txt":      phaseOther,
		"Album/Album.xml":       phaseOther,
	}
	for path, want := range cases {
		if got := filePhase(path); got != want {
			t.Errorf("%s: этап %d, ожидался %d", path, got, want)
		}
	}
}

func TestFormatETA(t *testing.T) {
	cases := []struct {
		rem, speed float64
		want       string
	}{
		{1e9, 0, "Неизвестно"},
		{1e9, 1, "Неизвестно"},
		{1e12, 2e6, "Неизвестно"},
		{1e6, 1e6, "меньше минуты"},
		{300e6, 1e6, "5 мин"},
		{3900e6, 1e6, "1 ч 05 мин"},
	}
	for _, c := range cases {
		if got := formatETA(c.rem, c.speed); got != c.want {
			t.Errorf("formatETA(%v, %v) = %q, ожидалось %q", c.rem, c.speed, got, c.want)
		}
	}
}

func TestAllZero(t *testing.T) {
	if !allZero(make([]byte, 1517)) || allZero([]byte("FILE")) || allZero(nil) {
		t.Fatal("allZero")
	}
}

func TestPrefetchCountedOnce(t *testing.T) {
	ti := &state.TorrentStatus{FileStats: []*state.TorrentFileStat{
		{Id: 1, Path: "A/Scans/Back.png", Length: 100},
		{Id: 2, Path: "A/Scans/Unselected.png", Length: 50},
	}}
	wrk := &Worker{ti: ti, fileIndices: []int{0}}
	markPrefetched(wrk, ti.FileStats[0])
	markPrefetched(wrk, ti.FileStats[0])
	markPrefetched(wrk, ti.FileStats[1])
	if got := wrk.downloadedBytes.Load(); got != 100 {
		t.Fatalf("засчитано %d байт, ожидалось 100", got)
	}
}
