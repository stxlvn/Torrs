package torr

import "testing"

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
