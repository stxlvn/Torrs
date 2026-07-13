package torr

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"torrsru/tgbot/torr/state"
)

// isCueSplitCandidate — форматы, для которых имеет смысл искать
// сопроводительный cue-sheet. Ограничено FLAC: это единственный формат из
// обрабатываемых AudioProcessor'ом (см. isProcessableAudio), для которого
// на практике встречается сценарий "альбом одним файлом + .cue". WAV/APE в
// текущем пайплайне вообще не проходят через AudioProcessor.
func isCueSplitCandidate(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".flac")
}

// hasSiblingCueFile — быстрая проверка на диске (без скачивания и разбора)
// наличия .cue файла в той же папке, что и diskPath. Используется
// uploadFileFromDisk, чтобы решить, пропускать ли порог safePartSize для
// файла — сам разбор cue и диалог с пользователем остаются на стороне
// AudioProcessor (tgbot.ProcessAudioFile), который может обнаружить, что
// cue на самом деле нет/не разбирается, и запросить откат на 7z.
func hasSiblingCueFile(diskPath string) bool {
	dir := filepath.Dir(diskPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".cue") {
			return true
		}
	}
	return false
}

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
		if !isCueSplitCandidate(f.Path) {
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
		dir := filepath.Dir(strings.TrimPrefix(f.Path, "/"))
		if dirsWithCandidate[dir] {
			cueFiles = append(cueFiles, f)
		}
	}

	for _, cueFile := range cueFiles {
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
func fetchCueToTmp(wrk *Worker, cueFile *state.TorrentFileStat) error {
	torrFile, err := NewTorrFile(wrk, cueFile)
	if err != nil {
		return err
	}
	defer torrFile.Close()

	data, err := io.ReadAll(torrFile)
	if err != nil {
		return err
	}

	relPath := strings.TrimPrefix(cueFile.Path, "/")
	fullPath := filepath.Join(wrk.tmpDir, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return err
	}
	log.Printf("[cue] найден cue-sheet %q, скачано %d байт -> %s", cueFile.Path, len(data), fullPath)
	return os.WriteFile(fullPath, data, 0644)
}
