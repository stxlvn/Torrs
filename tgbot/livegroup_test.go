package tgbot

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLiveAudioMediaGroup(t *testing.T) {
	chat := os.Getenv("TORRS_LIVE_CHAT")
	if chat == "" {
		t.Skip("TORRS_LIVE_CHAT не задан")
	}
	botToken, botAPIHost = os.Getenv("BOT_TOKEN"), "http://127.0.0.1:8082"
	dir := t.TempDir()
	var items []readyTrack
	cover := filepath.Join(dir, "c.jpg")
	exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=size=320x320", "-frames:v", "1", cover).Run()
	coverBytes, _ := os.ReadFile(cover)
	for i, f := range []string{"440", "660"} {
		p := filepath.Join(dir, "t"+f+".m4a")
		if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=frequency="+f+":duration=3", "-c:a", "alac", p).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		items = append(items, readyTrack{Path: p, Title: "Тест группы " + string(rune('A'+i)), Performer: "Torrs", Duration: 3, Cover: coverBytes})
	}
	msgs, err := sendAudioMediaGroup(chat, items)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.Audio == nil || m.Audio.Thumbnail == nil {
			t.Errorf("сообщение %d без обложки: %+v", m.ID, m.Audio)
		} else {
			t.Logf("msg %d: %s — превью %dx%d, file_id=%.20s…, альбом %s", m.ID, m.Audio.Title, m.Audio.Thumbnail.Width, m.Audio.Thumbnail.Height, m.Audio.FileID, m.AlbumID)
		}
	}
}
