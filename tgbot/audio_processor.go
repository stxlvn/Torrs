package tgbot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dhowden/tag"
	tele "gopkg.in/telebot.v4"
)

func SendSmartFile(c tele.Context, filePath string) error {
	ext := strings.ToLower(filepath.Ext(filePath))
	isAudio := false
	audioExts := []string{".mp3", ".flac", ".m4a", ".ogg", ".wav"}
	
	for _, aExt := range audioExts {
		if ext == aExt {
			isAudio = true
			break
		}
	}

	if !isAudio {
		return c.Send(&tele.Document{File: tele.FromDisk(filePath)})
	}

	// Логика для Аудио
	f, err := os.Open(filePath)
	if err != nil {
		return c.Send(&tele.Document{File: tele.FromDisk(filePath)})
	}
	defer f.Close()

	m, err := tag.ReadFrom(f)
	if err != nil {
		return c.Send(&tele.Audio{File: tele.FromDisk(filePath)})
	}

	audio := &tele.Audio{
		File:      tele.FromDisk(filePath),
		Caption:   "",
		Title:     m.Title(),
		Performer: m.Artist(),
	}

	// Проверка обложки
	if p := m.Picture(); p != nil {
		tmpCover := filepath.Join(os.TempDir(), fmt.Sprintf("cover_%d.%s", c.Sender().ID, p.Ext))
		if err := os.WriteFile(tmpCover, p.Data, 0644); err == nil {
			audio.Thumbnail = &tele.Photo{File: tele.FromDisk(tmpCover)}
			// Удаляем обложку после отправки (через defer или вручную ниже)
		}
	}

	return c.Send(audio)
}
