package torr

import (
	"strings"
	"testing"
)

func TestStatusViewEscapesAndRenders(t *testing.T) {
	v := statusView{
		Title: "AC/DC <Live> & Co", Hash: "abc", FilesDone: 3, FilesTotal: 22, Total: 1300 << 20,
		DownDone: 540 << 20, Speed: "2.4 MB/с", Peers: "3 (3/40)", ETA: "6 мин", DownloadPhase: true,
		UpDone: 260 << 20, AudioStage: "🎼 Нарезка по cue: a.flac — трек 2 из 9",
	}
	for name, out := range map[string]string{"rich": v.richHTML(), "classic": v.classicHTML()} {
		if strings.Contains(out, "<Live>") || !strings.Contains(out, "&lt;Live&gt; &amp; Co") {
			t.Errorf("%s: title not escaped: %s", name, out)
		}
		if strings.Contains(out, "%!") {
			t.Errorf("%s: format error: %s", name, out)
		}
		if !strings.Contains(out, "6 мин") || !strings.Contains(out, "трек 2 из 9") {
			t.Errorf("%s: missing ETA or audio stage: %s", name, out)
		}
	}
	t.Log(v.richHTML())
}
