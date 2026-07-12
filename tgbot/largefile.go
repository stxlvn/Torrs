package tgbot

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	tele "gopkg.in/telebot.v4"
	"torrsru/tgbot/torr"
)

// ProgressReader перехватывает поток данных при выгрузке в ТГ
type ProgressReader struct {
	io.Reader
	onProgress func(readBytes int)
}

func (pr *ProgressReader) Read(p []byte) (n int, err error) {
	n, err = pr.Reader.Read(p)
	if n > 0 && pr.onProgress != nil {
		pr.onProgress(n)
	}
	return
}

// maxVolumeRetries — сколько раз повторить отправку одного тома при сетевой
// ошибке. Раньше отправка тома делалась один раз без ретраев (в отличие от
// manager.go, где обычные файлы отправляются через sendWithRetry) — сбой
// сети на многогигабайтном томе на 8-й минуте передачи заваливал всю
// задачу целиком, вынуждая начинать архивацию и выгрузку заново.
const maxVolumeRetries = 5

// sendVolumeWithRetry отправляет один том архива, повторяя попытку при
// сетевых сбоях. Файл переоткрывается на каждой попытке (io.Reader уже
// частично прочитан после неудачной отправки), а uploadedSize сбрасывается
// к состоянию до этой попытки, чтобы прогресс-бар не показывал байты,
// которые на самом деле не дошли до Telegram.
func sendVolumeWithRetry(c tele.Context, partPath string, i, totalParts int, isCancelled func() bool, onProgress func(int), uploadedSize *int64, baseline int64) error {
	var lastErr error
	for attempt := 1; attempt <= maxVolumeRetries; attempt++ {
		if isCancelled() {
			return fmt.Errorf("выгрузка отменена пользователем")
		}

		*uploadedSize = baseline

		partFile, err := os.Open(partPath)
		if err != nil {
			log.Printf("[largefile] том %d/%d: открытие FAILED: %v", i+1, totalParts, err)
			return fmt.Errorf("открытие тома %d: %w", i+1, err)
		}

		pr := &ProgressReader{Reader: partFile, onProgress: onProgress}
		doc := &tele.Document{
			FileName: filepath.Base(partPath),
			File:     tele.FromReader(pr),
			Caption:  fmt.Sprintf("Том %d из %d", i+1, totalParts),
		}

		partStart := time.Now()
		_, err = c.Bot().Send(c.Recipient(), doc)
		partFile.Close()

		if err == nil {
			log.Printf("[largefile] том %d/%d: отправлен за %v (попытка %d/%d)", i+1, totalParts, time.Since(partStart), attempt, maxVolumeRetries)
			return nil
		}

		lastErr = err
		log.Printf("[largefile] том %d/%d: попытка %d/%d FAILED после %v: %v", i+1, totalParts, attempt, maxVolumeRetries, time.Since(partStart), err)
		if attempt < maxVolumeRetries {
			time.Sleep(5 * time.Second)
		}
	}
	return lastErr
}

func ProcessLargeFile(c tele.Context, filePath string, fileSize int64, fileName string, hash string, statusMsg *tele.Message, isCancelled func() bool) error {
	log.Printf("ProcessLargeFile: START file=%s size=%d", filePath, fileSize)
	if isCancelled == nil {
		isCancelled = func() bool { return false }
	}

	title := hash
	if ti, _ := torr.GetTorrentInfo(hash); ti != nil && ti.Title != "" {
		title = ti.Title
	}

	generateMsg := func(archProgress, upProgress float64, archStatus, upStatus string) string {
		return fmt.Sprintf(
			"🚀 <b>Обработка торрента...</b>\n\n"+
				"💿 <b>Название:</b> %s\n\n"+
				"📥 <b>Скачивание на сервер:</b>\n"+
				"Прогресс: [%s] 100.00%%\n"+
				"✅ <i>Успешно завершено</i>\n\n"+
				"📦 <b>Архивация (7-Zip):</b>\n"+
				"Прогресс: [%s] %.2f%%\n"+
				"%s\n\n"+
				"📤 <b>Выгрузка в Telegram:</b>\n"+
				"Прогресс: [%s] %.2f%%\n"+
				"%s\n\n"+
				"⚙️ <code>%s</code>",
			title,
			progressBar(100.0),
			progressBar(archProgress), archProgress, archStatus,
			progressBar(upProgress), upProgress, upStatus,
			hash,
		)
	}

	var kbd *tele.ReplyMarkup
	if statusMsg != nil {
		kbd = statusMsg.ReplyMarkup
	}

	tmpDir, err := os.MkdirTemp("", "7z_out_*")
	if err != nil {
		return fmt.Errorf("создание временной папки: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	baseExt := filepath.Ext(fileName)
	baseName := strings.TrimSuffix(fileName, baseExt)
	archivePath := filepath.Join(tmpDir, baseName+".7z")

	log.Printf("[largefile] %s: запуск 7z архивации -> %s", filePath, archivePath)
	archStart := time.Now()
	cmd := exec.Command("7z", "a", "-v1900m", "-mx0", archivePath, filePath)
	err = cmd.Start()
	if err != nil {
		log.Printf("[largefile] %s: запуск 7z FAILED: %v", filePath, err)
		return fmt.Errorf("ошибка запуска 7z: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var lastUpdate time.Time

	// ФАЗА 1: Архивация
ArchivingLoop:
	for {
		if isCancelled() {
			log.Printf("[largefile] %s: архивация отменена пользователем после %v", filePath, time.Since(archStart))
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
			<-done
			return fmt.Errorf("архивация отменена пользователем")
		}
		select {
		case err := <-done:
			if err != nil {
				log.Printf("[largefile] %s: 7z FAILED после %v: %v", filePath, time.Since(archStart), err)
				return fmt.Errorf("ошибка архивации 7z: %w", err)
			}
			log.Printf("[largefile] %s: архивация завершена за %v", filePath, time.Since(archStart))
			break ArchivingLoop
		case <-ticker.C:
			if statusMsg == nil || time.Since(lastUpdate) < 3*time.Second {
				continue
			}

			var currentSize int64
			filepath.Walk(tmpDir, func(_ string, info os.FileInfo, e error) error {
				if e == nil && !info.IsDir() {
					currentSize += info.Size()
				}
				return nil
			})

			progress := float64(currentSize) / float64(fileSize) * 100.0
			if progress > 100 {
				progress = 100
			}

			text := generateMsg(progress, 0, "⏳ <i>Нарезка томов...</i>", "⏳ <i>Ожидание файлов...</i>")
			_, _ = c.Bot().Edit(statusMsg, text, kbd, tele.ModeHTML)
			lastUpdate = time.Now()
		}
	}

	matches, err := filepath.Glob(archivePath + ".*")
	if err != nil || len(matches) == 0 {
		log.Printf("[largefile] %s: файлы архива не найдены (glob err=%v)", filePath, err)
		return fmt.Errorf("файлы архива не найдены")
	}

	sort.Strings(matches)
	totalParts := len(matches)
	log.Printf("[largefile] %s: получено томов=%d", filePath, totalParts)

	var totalArchiveSize int64
	for _, match := range matches {
		stat, _ := os.Stat(match)
		totalArchiveSize += stat.Size()
	}

	var uploadedSize int64

	// ФАЗА 2: Выгрузка томов с живым статус-баром
	for i, partPath := range matches {
		if isCancelled() {
			log.Printf("[largefile] %s: выгрузка отменена пользователем на томе %d/%d", filePath, i+1, totalParts)
			return fmt.Errorf("выгрузка отменена пользователем")
		}

		partSizeBeforeAttempt := uploadedSize
		err := sendVolumeWithRetry(c, partPath, i, totalParts, isCancelled, func(readBytes int) {
			uploadedSize += int64(readBytes)
			if statusMsg != nil && time.Since(lastUpdate) >= 3*time.Second {
				progress := float64(uploadedSize) / float64(totalArchiveSize) * 100.0
				if progress > 100 {
					progress = 100
				}

				archStatus := fmt.Sprintf("✅ <i>Успешно завершено (%d томов)</i>", totalParts)
				upStatus := fmt.Sprintf("🎵 <i>Отправляется том %d из %d... (%s / %s)</i>", i+1, totalParts, humanize.Bytes(uint64(uploadedSize)), humanize.Bytes(uint64(totalArchiveSize)))

				text := generateMsg(100.0, progress, archStatus, upStatus)
				c.Bot().Edit(statusMsg, text, kbd, tele.ModeHTML)
				lastUpdate = time.Now()
			}
		}, &uploadedSize, partSizeBeforeAttempt)
		if err != nil {
			return fmt.Errorf("отправка тома %d: %w", i+1, err)
		}

		if i < totalParts-1 {
			time.Sleep(2 * time.Second)
		}
	}
	log.Printf("[largefile] %s: все тома отправлены (%d), общее время %v", filePath, totalParts, time.Since(archStart))

	// ФАЗА 3: Завершение
	if statusMsg != nil {
		archStatus := fmt.Sprintf("✅ <i>Успешно завершено (%d томов)</i>", totalParts)
		upStatus := "✅ <i>Все тома отправлены</i>\n\n" +
			"📋 <b>Как распаковать:</b>\n" +
			"1. Скачайте все тома (<code>.001</code>, <code>.002</code>...) в одну папку.\n" +
			"2. Нажмите правой кнопкой по первому файлу (<code>.001</code>).\n" +
			"3. Выберите «Распаковать» в WinRAR или 7-Zip."

		text := generateMsg(100.0, 100.0, archStatus, upStatus)
		_, _ = c.Bot().Edit(statusMsg, text, kbd, tele.ModeHTML)
	}

	return nil
}

func progressBar(percent float64) string {
	const size = 10
	filled := int(percent / 100 * size)
	if filled > size {
		filled = size
	}
	if filled < 0 {
		filled = 0
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", size-filled)
	return bar
}
