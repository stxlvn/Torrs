package tgbot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/dhowden/tag"
	tele "gopkg.in/telebot.v4"
	"torrsru/tgbot/torr"
)

type PendingCover struct {
	AudioPath string
	Hash      string
	Images    []torrCoverImage // список картинок из торрента (или nil, если нет)
}

type torrCoverImage struct {
	Name  string // имя файла для кнопки
	Index int    // индекс в FileStats
}

var pendingCovers sync.Map

// ProcessAudioFile – основная публичная функция обработки аудио.
func ProcessAudioFile(c tele.Context, filePath string, hash string) error {
	ext := strings.ToLower(filepath.Ext(filePath))
	if ext != ".mp3" && ext != ".flac" && ext != ".m4a" && ext != ".ogg" {
		return nil
	}

	artist, title, duration, hasCover, coverData := readAudioInfo(filePath)
	fmt.Printf("[audio] %s: artist=%q title=%q duration=%v hasCover=%v\n", filePath, artist, title, duration, hasCover)

	if hasCover {
		return sendAudio(c, filePath, artist, title, duration, coverData)
	}

	chatID := c.Sender().ID
	key := fmt.Sprintf("%d_%s", chatID, hash)

	// Если уже ожидается выбор обложки для этого торрента, не спамим – отправляем без обложки
	if _, exists := pendingCovers.Load(key); exists {
		return sendAudio(c, filePath, artist, title, duration, nil)
	}

	// Ищем картинки в виртуальной структуре торрента
	ti, err := torr.GetTorrentInfo(hash)
	if err != nil {
		return sendAudio(c, filePath, artist, title, duration, nil)
	}

	var torrentImages []torrCoverImage
	for i, f := range ti.FileStats {
		ext := strings.ToLower(filepath.Ext(f.Path))
		if ext == ".jpg" || ext == ".jpeg" || ext == ".png" {
			torrentImages = append(torrentImages, torrCoverImage{
				Name:  filepath.Base(f.Path),
				Index: i,
			})
		}
	}

	if len(torrentImages) > 0 {
		return offerCoverSelection(c, hash, filePath, torrentImages, artist, title, duration, coverData)
	}
	return requestCustomCover(c, hash, filePath, artist, title, duration, coverData)
}

func readAudioInfo(filePath string) (artist, title string, duration int, hasCover bool, coverData []byte) {
	f, err := os.Open(filePath)
	if err != nil {
		artist, title = parseFileName(filePath)
		return
	}
	defer f.Close()

	m, err := tag.ReadFrom(f)
	if err != nil {
		artist, title = parseFileName(filePath)
	} else {
		artist = m.Artist()
		title = m.Title()
		if artist == "" || title == "" {
			a, t := parseFileName(filePath)
			if artist == "" {
				artist = a
			}
			if title == "" {
				title = t
			}
		}
		if pic := m.Picture(); pic != nil {
			hasCover = true
			coverData = pic.Data
		}
	}

	duration = getDurationFFprobe(filePath)
	return
}

func parseFileName(filePath string) (artist, title string) {
	base := filepath.Base(filePath)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	parts := strings.SplitN(name, " - ", 2)
	if len(parts) == 2 {
		artist = strings.TrimSpace(parts[0])
		title = strings.TrimSpace(parts[1])
	} else {
		title = strings.TrimSpace(name)
	}
	return
}

func getDurationFFprobe(filePath string) int {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "json",
		filePath,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	var info struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return 0
	}
	d, _ := strconv.ParseFloat(info.Format.Duration, 64)
	return int(d)
}

func writeAudioTags(filePath, artist, title, coverPath string) error {
	tmpFile := filePath + ".tagged"
	args := []string{
		"-i", filePath,
		"-i", coverPath,
		"-map", "0:a",
		"-map", "1:v",
		"-c", "copy",
		"-metadata", fmt.Sprintf("artist=%s", artist),
		"-metadata", fmt.Sprintf("title=%s", title),
		"-disposition:v", "attached_pic",
		tmpFile,
	}
	cmd := exec.Command("ffmpeg", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg error: %v, output: %s", err, out)
	}

	info, err := os.Stat(tmpFile)
	if err != nil || info.Size() == 0 {
		return fmt.Errorf("ошибка при создании файла с тегами")
	}

	if err := os.Rename(tmpFile, filePath); err != nil {
		return err
	}
	return nil
}

func sendAudio(c tele.Context, filePath, artist, title string, duration int, coverData []byte) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	audio := &tele.Audio{
		File:      tele.FromReader(file),
		Title:     title,
		Performer: artist,
		Duration:  duration,
	}
	if len(coverData) > 0 {
		audio.Thumbnail = &tele.Photo{
			File: tele.FromReader(bytes.NewReader(coverData)),
		}
	}
	_, err = c.Bot().Send(c.Recipient(), audio)
	return err
}

func offerCoverSelection(c tele.Context, hash, audioPath string, images []torrCoverImage,
	artist, title string, duration int, existingCover []byte) error {

	chatID := c.Sender().ID
	key := fmt.Sprintf("%d_%s", chatID, hash)
	pendingCovers.Store(key, &PendingCover{AudioPath: audioPath, Hash: hash, Images: images})

	markup := &tele.ReplyMarkup{}
	var rows []tele.Row
	for _, img := range images {
		btn := markup.Data(img.Name, "\fcover", hash, strconv.Itoa(img.Index))
		rows = append(rows, markup.Row(btn))
	}
	rows = append(rows, markup.Row(
		markup.Data("▶️ Без обложки", "\fskip", hash),
		markup.Data("📤 Загрузить свою", "\fcovup", hash),
	))
	markup.Inline(rows...)

	return c.Send("🎵 Выберите обложку или пропустите:", markup)
}

func requestCustomCover(c tele.Context, hash, audioPath string,
	artist, title string, duration int, existingCover []byte) error {

	chatID := c.Sender().ID
	key := fmt.Sprintf("%d_%s", chatID, hash)
	pendingCovers.Store(key, &PendingCover{AudioPath: audioPath, Hash: hash, Images: nil})

	markup := &tele.ReplyMarkup{}
	markup.Inline(
		markup.Row(markup.Data("▶️ Без обложки", "\fskip", hash)),
		markup.Row(markup.Data("📤 Загрузить свою", "\fcovup", hash)),
	)
	return c.Send("📎 Картинок в папке нет. Выберите действие:", markup)
}

func handleCoverSkip(c tele.Context, hash string) error {
	chatID := c.Sender().ID
	key := fmt.Sprintf("%d_%s", chatID, hash)
	val, ok := pendingCovers.Load(key)
	if !ok {
		return c.Respond(&tele.CallbackResponse{Text: "Данные устарели", ShowAlert: true})
	}
	pc := val.(*PendingCover)
	artist, title, duration, _, coverData := readAudioInfo(pc.AudioPath)
	err := sendAudio(c, pc.AudioPath, artist, title, duration, coverData)
	if err == nil {
		pendingCovers.Delete(key)
	}
	return err
}

func handleCoverSelection(c tele.Context, hash string, imgIndex int) error {
	chatID := c.Sender().ID
	key := fmt.Sprintf("%d_%s", chatID, hash)
	val, ok := pendingCovers.Load(key)
	if !ok {
		return c.Respond(&tele.CallbackResponse{Text: "Данные устарели", ShowAlert: true})
	}
	pc := val.(*PendingCover)

	// Скачиваем выбранный файл картинки из торрента
	coverPath, err := torr.DownloadTorrentFile(hash, imgIndex)
	if err != nil {
		return c.Respond(&tele.CallbackResponse{Text: "Ошибка загрузки обложки", ShowAlert: true})
	}
	defer os.Remove(coverPath) // временный файл, удалим после использования

	return applyCoverAndSend(c, pc.AudioPath, coverPath, hash)
}

func handleCustomCoverUpload(c tele.Context, hash string, msg *tele.Message) error {
	chatID := c.Sender().ID
	key := fmt.Sprintf("%d_%s", chatID, hash)
	val, ok := pendingCovers.Load(key)
	if !ok {
		return c.Send("Данные устарели. Начните сначала.")
	}
	pc := val.(*PendingCover)

	var file io.ReadCloser
	var err error
	if msg.Photo != nil {
		file, err = c.Bot().File(&msg.Photo.File)
	} else if msg.Document != nil {
		file, err = c.Bot().File(&msg.Document.File)
	} else {
		return c.Send("Пожалуйста, отправьте изображение.")
	}
	if err != nil {
		return err
	}
	defer file.Close()

	tmpPath := pc.AudioPath + ".cover.tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	defer out.Close()
	io.Copy(out, file)

	return applyCoverAndSend(c, pc.AudioPath, tmpPath, hash)
}

func applyCoverAndSend(c tele.Context, audioPath, coverPath, hash string) error {
	artist, title, duration, _, _ := readAudioInfo(audioPath)

	if err := writeAudioTags(audioPath, artist, title, coverPath); err != nil {
		return c.Send("⚠️ Не удалось записать теги: " + err.Error())
	}

	_, _, _, _, newCover := readAudioInfo(audioPath)
	err := sendAudio(c, audioPath, artist, title, duration, newCover)
	if err == nil {
		chatID := c.Sender().ID
		key := fmt.Sprintf("%d_%s", chatID, hash)
		pendingCovers.Delete(key)
		if strings.HasSuffix(coverPath, ".cover.tmp") {
			os.Remove(coverPath)
		}
	}
	return err
}
