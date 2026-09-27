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
