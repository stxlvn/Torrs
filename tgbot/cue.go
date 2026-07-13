package tgbot

import (
	"bufio"
	"errors"
	"fmt"
	"hash/crc32"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	tele "gopkg.in/telebot.v4"
)

// CueTrack — один трек из секции cue-sheet: номер, теги и стартовая позиция
// (INDEX 01) внутри СВОЕГО аудиофайла (см. CueFileSection). Конечная позиция
// явно не хранится — это начало следующего трека той же секции (или конец
// файла для последнего), считается в performCueSplit.
type CueTrack struct {
	Number    int
	Title     string
	Performer string
	Start     time.Duration
}

// CueFileSection — один блок FILE "..." внутри cue-sheet со своими треками.
// Один .cue может описывать НЕСКОЛЬКО физических аудиофайлов сразу (типичный
// случай — релиз "2×LP", где один общий cue содержит по блоку FILE на
// каждую пластинку, и тайминги TRACK/INDEX в каждом блоке отсчитываются от
// начала СВОЕГО файла, а не сквозным счётом).
type CueFileSection struct {
	AudioFile string
	Tracks    []CueTrack
}

// CueSheet — результат разбора .cue файла.
type CueSheet struct {
	Performer string // альбомный исполнитель — fallback для треков без своего PERFORMER
	Title     string // альбом
	Files     []CueFileSection
}

// SectionFor находит секцию, чей FILE соответствует базовому имени
// audioFileName (регистронезависимо, без учёта пути). Возвращает nil, если
// cue вообще не описывает такой файл — например, .cue в папке относится
// только к ОДНОМУ из нескольких lossless-файлов, лежащих рядом.
func (s *CueSheet) SectionFor(audioFileName string) *CueFileSection {
	base := strings.ToLower(filepath.Base(audioFileName))
	for i := range s.Files {
		if strings.ToLower(filepath.Base(s.Files[i].AudioFile)) == base {
			return &s.Files[i]
		}
	}
	// Единственная секция без явного совпадения имени по файлу — обычный
	// случай "один FILE на весь cue", где имя в FILE может не совпадать
	// с реальным именем на диске (например, cue написан под .wav, а в
	// раздаче лежит перекодированный .flac).
	if len(s.Files) == 1 {
		return &s.Files[0]
	}
	return nil
}

// decodeCueBytes приводит содержимое .cue к UTF-8. Многие cue-файлы (особенно
// со сборниками/русскими тегами) сохранены в Windows-1251 без BOM — попытка
// прочитать их как UTF-8 напрямую либо ломается на невалидных байтах, либо
// превращает кириллицу в кракозябры.
func decodeCueBytes(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	decoded, err := charmap.Windows1251.NewDecoder().Bytes(data)
	if err != nil {
		return string(data)
	}
	return string(decoded)
}

// parseCueTime разбирает время в формате cue-sheet: mm:ss:ff, где ff —
// кадры CDDA (1 кадр = 1/75 секунды), а не миллисекунды.
func parseCueTime(s string) (time.Duration, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, fmt.Errorf("некорректный формат времени %q", s)
	}
	mm, err1 := strconv.Atoi(parts[0])
	ss, err2 := strconv.Atoi(parts[1])
	ff, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, fmt.Errorf("некорректный формат времени %q", s)
	}
	return time.Duration(mm)*time.Minute + time.Duration(ss)*time.Second + time.Duration(ff)*time.Second/75, nil
}

// parseCueQuoted вытаскивает содержимое в кавычках; если кавычек нет
// (встречается в "неканоничных" cue), возвращает остаток строки как есть.
func parseCueQuoted(rest string) string {
	rest = strings.TrimSpace(rest)
	if len(rest) >= 2 && rest[0] == '"' && rest[len(rest)-1] == '"' {
		return rest[1 : len(rest)-1]
	}
	return rest
}

// parseCueFileName вытаскивает имя файла из строки вида
// FILE "имя файла.flac" WAVE — в кавычках, с завершающим типом (WAVE/
// BINARY/MP3...) после них. Если кавычек нет (неканоничный cue), отрезаем
// последнее слово (тип) и считаем остальное именем.
func parseCueFileName(rest string) string {
	rest = strings.TrimSpace(rest)
	if idx := strings.LastIndex(rest, "\""); idx > 0 {
		return parseCueQuoted(rest[:idx+1])
	}
	fields := strings.Fields(rest)
	if len(fields) > 1 {
		return strings.Join(fields[:len(fields)-1], " ")
	}
	return rest
}

// parseCueSheet разбирает содержимое .cue файла. Понимает базовый набор
// команд, достаточный для нарезки аудиофайлов на треки: FILE (может
// встречаться несколько раз — см. CueFileSection), PERFORMER и TITLE
// (альбомные и потрековые — потрековые переопределяют альбомные для
// соответствующего трека), TRACK NN AUDIO, INDEX 01 (начало трека). INDEX 00
// (пре-гэп) намеренно игнорируется — трек начинается с INDEX 01, как это
// принято у большинства плееров и рипперов.
func parseCueSheet(data []byte) (*CueSheet, error) {
	text := decodeCueBytes(data)
	sheet := &CueSheet{}
	var curFile *CueFileSection
	var curTrack *CueTrack

	flushTrack := func() {
		if curTrack != nil && curFile != nil {
			curFile.Tracks = append(curFile.Tracks, *curTrack)
		}
		curTrack = nil
	}
	flushFile := func() {
		flushTrack()
		if curFile != nil {
			sheet.Files = append(sheet.Files, *curFile)
		}
		curFile = nil
	}

	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		upper := strings.ToUpper(line)

		switch {
		case strings.HasPrefix(upper, "FILE "):
			flushFile()
			curFile = &CueFileSection{AudioFile: parseCueFileName(line[len("FILE "):])}
		case strings.HasPrefix(upper, "PERFORMER "):
			val := parseCueQuoted(line[len("PERFORMER "):])
			if curTrack != nil {
				curTrack.Performer = val
			} else {
				sheet.Performer = val
			}
		case strings.HasPrefix(upper, "TITLE "):
			val := parseCueQuoted(line[len("TITLE "):])
			if curTrack != nil {
				curTrack.Title = val
			} else {
				sheet.Title = val
			}
		case strings.HasPrefix(upper, "TRACK "):
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			num, err := strconv.Atoi(fields[1])
			if err != nil {
				continue
			}
			flushTrack()
			if curFile == nil {
				// TRACK встретился раньше FILE — не по стандарту, но
				// подстрахуемся безымянной секцией, чтобы не терять треки.
				curFile = &CueFileSection{}
			}
			curTrack = &CueTrack{Number: num}
		case strings.HasPrefix(upper, "INDEX "):
			fields := strings.Fields(line)
			if len(fields) < 3 || curTrack == nil || fields[1] != "01" {
				continue
			}
			t, err := parseCueTime(fields[2])
			if err != nil {
				continue
			}
			curTrack.Start = t
		}
	}
	flushFile()

	if len(sheet.Files) == 0 {
		return nil, fmt.Errorf("cue-sheet не содержит треков")
	}
	return sheet, nil
}

// PendingCueSplit — решение по нарезке одного аудиофайла (одна секция
// cue-sheet), ожидающее ответа пользователя. В отличие от PendingCover, тут
// не нужна очередь "треков, пришедших до решения" — исходный файл ровно
// один и AudioProcessor вызывается по нему ровно один раз.
type PendingCueSplit struct {
	AudioPath      string
	Tracks         []CueTrack
	AlbumPerformer string // fallback-исполнитель для треков без своего PERFORMER
	Hash           string
	RootTmp        string
	PickerMsg      *tele.Message

	// Oversized/Fallback — см. ProcessAudioFile: если пользователь откажется
	// от нарезки, а файл превышает лимит одиночной отправки, вместо обычной
	// отправки нужно откатиться на 7z-архивацию.
	Oversized bool
	Fallback  func() error
}

var pendingCueSplits sync.Map

// findSiblingCueFiles возвращает пути ко всем .cue файлам в той же папке,
// что и audioPath (может быть несколько — например, отдельный .cue на
// каждый диск). Регистр расширения не важен (.cue/.CUE). На диске они
// оказываются благодаря prefetchCueSheets в tgbot/torr/cue.go, которая
// докачивает их заранее, даже если пользователь не выбирал их руками в
// файловом меню.
func findSiblingCueFiles(audioPath string) []string {
	dir := filepath.Dir(audioPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(e.Name()), ".cue") {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	return paths
}

// offerCueSplit читает и разбирает cue-файл, ищет в нём секцию для ИМЕННО
// этого audioPath (по имени файла в FILE) и, если в ней хотя бы 2 трека,
// спрашивает пользователя, нарезать ли. handled=true означает, что
// вызывающий код (ProcessAudioFile) должен вернуть управление немедленно —
// решение по этому файлу теперь асинхронное и придёт через callback
// (handleCueSplitConfirm/handleCueSplitDecline). handled=false — cue не
// разобрался/не описывает этот файл/содержит для него меньше 2 треков:
// вызывающий код должен попробовать следующий .cue из папки (если есть) или
// обработать файл как обычный одиночный трек.
func offerCueSplit(c tele.Context, audioPath, cuePath, hash, rootTmp string, oversized bool, fallback func() error) (handled bool, err error) {
	data, err := os.ReadFile(cuePath)
	if err != nil {
		log.Printf("[cue] %s: не удалось прочитать %s: %v", audioPath, cuePath, err)
		return false, nil
	}
	sheet, err := parseCueSheet(data)
	if err != nil {
		log.Printf("[cue] %s: не удалось разобрать %s: %v", audioPath, cuePath, err)
		return false, nil
	}
	section := sheet.SectionFor(audioPath)
	if section == nil {
		log.Printf("[cue] %s: %s не описывает этот файл (доступные FILE: %d шт.)", audioPath, cuePath, len(sheet.Files))
		return false, nil
	}
	if len(section.Tracks) < 2 {
		// Один "трек" на весь файл — нарезать нечего.
		log.Printf("[cue] %s: в %s для этого файла меньше 2 треков (%d), нарезка не нужна", audioPath, cuePath, len(section.Tracks))
		return false, nil
	}

	// Ключ и параметр callback'а — по КОНКРЕТНОМУ ФАЙЛУ, а не по папке: в
	// одной папке может лежать несколько независимых цельных FLAC (см.
	// CueFileSection), и у каждого — свой pending-выбор. Ключ по папке
	// заставил бы второй файл затирать состояние первого.
	fileHash := fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(audioPath)))
	key := fmt.Sprintf("%d_%s_%s", c.Sender().ID, hash, fileHash)

	pcs := &PendingCueSplit{
		AudioPath:      audioPath,
		Tracks:         section.Tracks,
		AlbumPerformer: sheet.Performer,
		Hash:           hash,
		RootTmp:        rootTmp,
		Oversized:      oversized,
		Fallback:       fallback,
	}
	pendingCueSplits.Store(key, pcs)

	// Файл превышает лимит одиночной отправки (см. ProcessAudioFile) — без
	// нарезки он всё равно уйдёт архивом, а не обычным аудиосообщением,
	// поэтому явно предупреждаем в подписи кнопки, а не молча подменяем
	// ожидаемое поведение "как есть".
	skipLabel := "▶️ Отправить как есть"
	if oversized {
		skipLabel = "📦 Отправить архивом (7z)"
	}
	markup := &tele.ReplyMarkup{}
	markup.Inline(
		markup.Row(markup.Data(fmt.Sprintf("🎼 Нарезать на %d треков", len(section.Tracks)), "\fcuesplit", hash, fileHash)),
		markup.Row(markup.Data(skipLabel, "\fcueskip", hash, fileHash)),
	)

	fileName := filepath.Base(audioPath)
	msgText := fmt.Sprintf("🎼 Для файла <b>%s</b> найден cue-sheet (%d треков). Нарезать на отдельные треки?", fileName, len(section.Tracks))
	sentMsg, sendErr := c.Bot().Send(c.Recipient(), msgText, markup, tele.ModeHTML)
	if sendErr != nil {
		log.Printf("[cue] %s: не удалось показать меню подтверждения: %v", audioPath, sendErr)
		pendingCueSplits.Delete(key)
		return false, nil
	}
	pcs.PickerMsg = sentMsg
	return true, nil
}

func popPendingCueSplit(c tele.Context, hash, fileHash string) (*PendingCueSplit, error) {
	key := fmt.Sprintf("%d_%s_%s", c.Sender().ID, hash, fileHash)
	val, ok := pendingCueSplits.LoadAndDelete(key)
	if !ok {
		return nil, errors.New("данные устарели")
	}
	return val.(*PendingCueSplit), nil
}

// handleCueSplitDecline — пользователь отказался от нарезки. Для обычного
// файла отправляем его целиком через обычный путь (конвертация в M4A,
// выбор обложки и т.д.). Для файла, превышающего лимит одиночной отправки
// (Oversized — сюда его пропустили только ради шанса нарезать по cue),
// вместо этого откатываемся на 7z-архивацию: обычный путь такой файл
// корректно не отправит.
func handleCueSplitDecline(c tele.Context, hash, fileHash string) error {
	pcs, err := popPendingCueSplit(c, hash, fileHash)
	if err != nil {
		return c.Respond(&tele.CallbackResponse{Text: "Данные устарели"})
	}
	if pcs.PickerMsg != nil {
		c.Bot().Delete(pcs.PickerMsg)
	}

	if pcs.Oversized {
		c.Respond(&tele.CallbackResponse{Text: "Архивирую"})
		// Порядок важен: сначала fallback (файл на диске ещё должен
		// существовать), потом completeAudioTask — иначе счётчик может
		// дойти до нуля и папка удалится до того, как архиватор прочитает
		// файл.
		fbErr := pcs.Fallback()
		completeAudioTask(pcs.RootTmp)
		return fbErr
	}

	c.Respond(&tele.CallbackResponse{Text: "Отправляю как есть"})
	return processAudioFileNormally(c, pcs.AudioPath, pcs.Hash, pcs.RootTmp)
}

// handleCueSplitConfirm — пользователь подтвердил нарезку.
func handleCueSplitConfirm(c tele.Context, hash, fileHash string) error {
	pcs, err := popPendingCueSplit(c, hash, fileHash)
	if err != nil {
		return c.Respond(&tele.CallbackResponse{Text: "Данные устарели"})
	}
	if pcs.PickerMsg != nil {
		c.Bot().Edit(pcs.PickerMsg, fmt.Sprintf("🎼 Нарезаю на %d треков...", len(pcs.Tracks)), tele.ModeHTML)
	}
	c.Respond(&tele.CallbackResponse{Text: "Нарезаю по CUE"})

	splitErr := performCueSplit(c, pcs)
	if pcs.PickerMsg != nil {
		c.Bot().Delete(pcs.PickerMsg)
	}
	completeAudioTask(pcs.RootTmp)
	if splitErr != nil {
		log.Printf("[cue] %s: нарезка завершилась с ошибками: %v", pcs.AudioPath, splitErr)
	}
	return splitErr
}

// cueAlbumCover достаёт обложку для всех треков нарезаемого файла разом
// (одна попытка, а не на трек): сперва пробуем вшитую в сам исходный FLAC,
// иначе — любую картинку из папки. Как и в обычном потоке (см.
// compressCoverBytes), результат сжимается до ≤200 КБ под превью Telegram.
func cueAlbumCover(audioPath string) []byte {
	_, _, _, hasCover, coverData := readAudioInfo(audioPath)
	if hasCover && len(coverData) > 0 {
		coverPath, err := saveCoverDataToTemp(coverData)
		if err == nil {
			defer os.Remove(coverPath)
			if compressed, cErr := compressCoverBytes(coverPath); cErr == nil {
				return compressed
			}
			return coverData
		}
	}

	images := findImagesInDir(filepath.Dir(audioPath))
	if len(images) > 0 {
		if compressed, err := compressCoverBytes(images[0]); err == nil {
			return compressed
		}
	}
	return nil
}

// performCueSplit нарезает исходный файл на треки по секции cue-sheet и
// отправляет каждый через sendAudio. Конец трека — это начало следующего (в
// пределах ТОЙ ЖЕ секции/файла) или конец файла для последнего; INDEX 00
// (пре-гэп) уже отброшен на этапе разбора.
func performCueSplit(c tele.Context, pcs *PendingCueSplit) error {
	totalDur := time.Duration(getDurationFFprobe(pcs.AudioPath)) * time.Second
	coverData := cueAlbumCover(pcs.AudioPath)

	var lastErr error
	for i, tr := range pcs.Tracks {
		end := totalDur
		if i+1 < len(pcs.Tracks) {
			end = pcs.Tracks[i+1].Start
		}
		if end <= tr.Start {
			log.Printf("[cue] %s: трек %d нулевой/отрицательной длительности (start=%v end=%v), пропуск", pcs.AudioPath, tr.Number, tr.Start, end)
			continue
		}

		performer := tr.Performer
		if performer == "" {
			performer = pcs.AlbumPerformer
		}
		title := tr.Title
		if title == "" {
			title = fmt.Sprintf("Track %d", tr.Number)
		}

		outPath, err := cutCueTrack(pcs.AudioPath, tr.Start, end, tr.Number)
		if err != nil {
			log.Printf("[cue] %s: не удалось нарезать трек %d (%v–%v): %v", pcs.AudioPath, tr.Number, tr.Start, end, err)
			lastErr = err
			continue
		}

		if err := sendAudio(c, outPath, performer, title, int((end-tr.Start).Seconds()), coverData); err != nil {
			log.Printf("[cue] %s: трек %d не отправлен: %v", pcs.AudioPath, tr.Number, err)
			lastErr = err
		}
	}
	return lastErr
}

// cutCueTrack вырезает [start, end) из srcPath и перекодирует в ALAC/M4A —
// как и одиночные FLAC (см. convertToM4A), т.к. Bot API принимает для
// sendAudio только .mp3/.m4a. -ss ДО -i даёт быстрый seek по входному
// файлу; -to при этом трактуется как абсолютная позиция в исходном
// таймлайне (а не относительно точки seek), что и нужно — Start/End уже
// абсолютные тайминги внутри СВОЕГО файла из cue-sheet. trackNum входит в
// имя выходного файла, а не в порядковый номер внутри секции, поэтому имена
// разных файлов одной папки (см. CueFileSection) не конфликтуют между
// собой только благодаря тому, что резка каждого файла идёт в его же
// каталоге — collision тут невозможен, т.к. номера треков в cue уникальны
// по всему документу (see TRACK NN сквозная нумерация в примере ICE MC).
func cutCueTrack(srcPath string, start, end time.Duration, trackNum int) (string, error) {
	outPath := filepath.Join(filepath.Dir(srcPath), fmt.Sprintf("cue_track_%02d.m4a", trackNum))
	args := []string{
		"-ss", formatFFmpegTime(start),
		"-to", formatFFmpegTime(end),
		"-i", srcPath,
		"-map", "0:a",
		"-c:a", "alac",
		"-movflags", "+faststart",
		"-vn",
		outPath,
	}
	start2 := time.Now()
	cmd := exec.Command("ffmpeg", args...)
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start2)
	if err != nil {
		os.Remove(outPath)
		log.Printf("[ffmpeg] cutCueTrack: трек %d FAILED after %v: %v, output: %s", trackNum, elapsed, err, out)
		return "", fmt.Errorf("ffmpeg error: %v, output: %s", err, out)
	}
	log.Printf("[ffmpeg] cutCueTrack: трек %d OK after %v -> %s", trackNum, elapsed, outPath)
	return outPath, nil
}

func formatFFmpegTime(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	ms := int(d.Milliseconds()) % 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}
