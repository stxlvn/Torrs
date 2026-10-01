package torr

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"torrsru/tgbot/torr/state"
)

// prefetchCueSheets ищет для каждой выбранной "цельноальбомной" FLAC-дорожки
// сопроводительные .cue в той же папке торрента — даже если пользователь их
// не выбирал руками в файловом меню — и докачивает их рядом на диск, ДО
// того как AudioProcessor увидит сам аудиофайл. Так AudioProcessor может
// просто проверить наличие .cue-файлов на диске, ничего не зная о структуре
// торрента и не требуя изменения своей сигнатуры.
//
// Раньше папка считалась кандидатом на нарезку, только если в ней ровно
// один lossless-файл — это ломалось на релизах вида "2xLP одним архивом":
// два цельных FLAC (по одному на пластинку) в одной папке при одном .cue,
// который на самом деле относится только к ОДНОМУ из них (see FILE-строка
// внутри самого cue). Подсчёт файлов в папке не мог этого различить и молча
// пропускал такую папку целиком. Теперь качаются ВСЕ .cue из папки, где
// есть хотя бы один FLAC-кандидат, а сопоставление конкретному файлу (по
// FILE-строке cue) делает уже AudioProcessor на стороне tgbot, где cue
// разбирается по-настоящему.
func prefetchCueSheets(wrk *Worker) {
	dirsWithCandidate := make(map[string]bool)
	for _, fi := range wrk.fileIndices {
		f := wrk.ti.FileStats[fi]
		if !isProcessableAudio(f.Path) {
			continue
		}
		dir := filepath.Dir(strings.TrimPrefix(f.Path, "/"))
		dirsWithCandidate[dir] = true
	}
	if len(dirsWithCandidate) == 0 {
		return
	}

	var cueFiles []*state.TorrentFileStat
	for _, f := range wrk.ti.FileStats {
		if !strings.EqualFold(filepath.Ext(f.Path), ".cue") {
			continue
		}
		cueDir := filepath.Dir(strings.TrimPrefix(f.Path, "/"))
		for dir := range dirsWithCandidate {
			if dir == cueDir || cueDir == "." || strings.HasPrefix(dir, cueDir+"/") {
				cueFiles = append(cueFiles, f)
				break
			}
		}
	}

	for _, cueFile := range cueFiles {
		if wrk.isCancelled.Load() {
			return
		}
		if err := fetchCueToTmp(wrk, cueFile); err != nil {
			log.Printf("[cue] не удалось скачать %q: %v", cueFile.Path, err)
		}
	}
}

// fetchCueToTmp скачивает .cue файл целиком (обычно единицы-десятки КБ) и
// кладёт его на диск рядом с тем местом, куда позже ляжет сам аудиофайл —
// вне учёта wrk.downloadedBytes/fileIndices, т.к. это служебный файл, а не
// часть задачи, которую нужно показывать в прогрессе или выгружать в чат
// как есть.
func readTorrFileOnce(wrk *Worker, f *state.TorrentFileStat) ([]byte, error) {
	torrFile, err := NewTorrFile(wrk, f)
	if err != nil {
		return nil, err
	}
	defer torrFile.Close()
	return io.ReadAll(torrFile)
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return len(b) > 0
}

// fetchCueToTmp скачивает .cue; при повторном добавлении торрента сразу
// после отмены TorrServer отдавал маленькие файлы нулями — такой ответ
// перезапрашивается.
func fetchCueToTmp(wrk *Worker, cueFile *state.TorrentFileStat) error {
	var data []byte
	var err error
	for attempt := 1; attempt <= 5; attempt++ {
		data, err = readTorrFileOnce(wrk, cueFile)
		if err == nil && !allZero(data) {
			break
		}
		if wrk.isCancelled.Load() || errors.Is(err, ERR_STOPPED) {
			return ERR_STOPPED
		}
		reason := "получены одни нули"
		if err != nil {
			reason = err.Error()
		}
		log.Printf("[cue] %q: попытка %d/5 — %s, повтор", cueFile.Path, attempt, reason)
		time.Sleep(time.Duration(attempt) * 2 * time.Second)
	}
	if err != nil {
		return err
	}
	if allZero(data) {
		return fmt.Errorf("TorrServer отдал вместо cue одни нули")
	}

	relPath := strings.TrimPrefix(cueFile.Path, "/")
	fullPath := filepath.Join(wrk.tmpDir, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return err
	}
	log.Printf("[cue] найден cue-sheet %q, скачано %d байт -> %s", cueFile.Path, len(data), fullPath)
	return os.WriteFile(fullPath, data, 0644)
}
