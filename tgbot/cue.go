package tgbot

import (
	"bufio"
	"context"
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

	"torrsru/db"
	"torrsru/tgbot/userbot"
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
	FileID         int // id исходного файла в торренте — для ключа кэша file_id per-трек
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

// CueTrackMeta — метаданные ЕДИНСТВЕННОГО трека cue-секции файла, которому
// не нужна нарезка (см. offerCueSplit, len(section.Tracks) < 2): PERFORMER/
// TITLE из самого cue надёжнее угадывания по имени файла (см. parseFileName
// в audio.go), когда в самом аудиофайле тегов нет — особенно для vinyl-
// side/многодисковых релизов, где имя файла содержит служебный суффикс
// вроде "SideA", а не реальное название трека.
type CueTrackMeta struct {
	Artist string
	Title  string
}

// offerCueSplit читает и разбирает cue-файл, ищет в нём секцию для ИМЕННО
// этого audioPath (по имени файла в FILE) и, если в ней хотя бы 2 трека,
// спрашивает пользователя, нарезать ли. handled=true означает, что
// вызывающий код (ProcessAudioFile) должен вернуть управление немедленно —
// решение по этому файлу теперь асинхронное и придёт через callback
// (handleCueSplitConfirm/handleCueSplitDecline, либо, для cue с несколькими
// FILE-секциями — handleCueGroupSplitConfirm/handleCueGroupSplitDecline, см.
// offerCueGroupSplit). handled=false — cue не разобрался/не описывает этот
// файл/содержит для него меньше 2 треков: вызывающий код должен попробовать
// следующий .cue из папки (если есть) или обработать файл как обычный
// одиночный трек — meta, если не nil, несёт PERFORMER/TITLE единственного
// трека секции для этого случая (см. CueTrackMeta).
func offerCueSplit(c tele.Context, audioPath, cuePath, hash, rootTmp string, fileID int, oversized bool, fallback func() error) (handled bool, meta *CueTrackMeta, err error) {
	data, err := os.ReadFile(cuePath)
	if err != nil {
		log.Printf("[cue] %s: не удалось прочитать %s: %v", audioPath, cuePath, err)
		return false, nil, nil
	}
	sheet, err := parseCueSheet(data)
	if err != nil {
		log.Printf("[cue] %s: не удалось разобрать %s: %v", audioPath, cuePath, err)
		return false, nil, nil
	}
	section := sheet.SectionFor(audioPath)
	if section == nil {
		log.Printf("[cue] %s: %s не описывает этот файл (доступные FILE: %d шт.)", audioPath, cuePath, len(sheet.Files))
		return false, nil, nil
	}
	if len(section.Tracks) < 2 {
		// Один "трек" на весь файл — нарезать нечего.
		log.Printf("[cue] %s: в %s для этого файла меньше 2 треков (%d), нарезка не нужна", audioPath, cuePath, len(section.Tracks))
		if len(section.Tracks) == 1 {
			tr := section.Tracks[0]
			performer := tr.Performer
			if performer == "" {
				performer = sheet.Performer
			}
			return false, &CueTrackMeta{Artist: performer, Title: tr.Title}, nil
		}
		return false, nil, nil
	}

	// Сколько секций этого cue вообще годятся для нарезки (>=2 трека) —
	// один .cue может описывать НЕСКОЛЬКО физических файлов сразу (см.
	// CueFileSection, типичный случай — релиз "2×LP"). Если такая секция
	// ровно одна — обычный случай, ведём себя как раньше (см. ниже). Если
	// больше одной — нужно ОДНО общее сообщение на все файлы сразу, а не
	// отдельный запрос на каждый (см. offerCueGroupSplit): пользователь
	// один раз решает "резать всё" или "отправить всё как есть", даже если
	// не все физические файлы группы ещё докачались.
	var qualifying int
	for i := range sheet.Files {
		if len(sheet.Files[i].Tracks) >= 2 {
			qualifying++
		}
	}
	if qualifying > 1 {
		handled, err := offerCueGroupSplit(c, audioPath, cuePath, sheet, hash, rootTmp, fileID, oversized, fallback)
		return handled, nil, err
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
		FileID:         fileID,
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
		return false, nil, nil
	}
	pcs.PickerMsg = sentMsg
	return true, nil, nil
}

// pendingCueGroupFile — одна физическая секция cue внутри общего группового
// решения (см. PendingCueGroup). Arrived/DiskPath заполняются, когда ИМЕННО
// этот физический файл реально докачается и дойдёт до offerCueGroupSplit —
// до этого момента запись существует только по данным самого cue (текст
// FILE-строки + список треков), без привязки к диску.
type pendingCueGroupFile struct {
	AudioFile string // имя файла как оно указано в FILE-строке cue
	Tracks    []CueTrack

	Arrived   bool
	Done      bool // решение уже применено к этому файлу
	DiskPath  string
	FileID    int
	Oversized bool
	Fallback  func() error
}

// PendingCueGroup — общее решение "резать/не резать" на ВЕСЬ cue, который
// описывает несколько физических файлов сразу (см. pendingCueGroupFile).
// В отличие от PendingCueSplit, здесь одно решение применяется к нескольким
// файлам, часть которых в момент показа меню может ещё не быть на диске —
// они докачаются позже (скачивание строго последовательное, см. runPipeline
// в tgbot/torr/manager.go) и, найдя Decided уже true, применят решение
// сразу, без повторного вопроса (см. offerCueGroupSplit).
type PendingCueGroup struct {
	CuePath        string
	Files          []*pendingCueGroupFile
	AlbumPerformer string
	Hash           string
	RootTmp        string
	PickerMsg      *tele.Message

	Decided   bool
	Confirmed bool

	mu sync.Mutex
}

var pendingCueGroups sync.Map // key: chatID_hash_crc32(cuePath) -> *PendingCueGroup

// offerCueGroupSplit — версия offerCueSplit для cue с несколькими FILE-
// секциями (>=2 трека каждая): вместо отдельного вопроса на каждый
// физический файл показывает ОДНО общее сообщение при первом же таком
// файле, что дошёл до этой функции, и ждёт общего решения — остальные
// файлы группы, дойдя сюда позже (или раньше, для уже прибывших),
// подхватывают уже принятое решение автоматически.
func offerCueGroupSplit(c tele.Context, audioPath, cuePath string, sheet *CueSheet, hash, rootTmp string, fileID int, oversized bool, fallback func() error) (handled bool, err error) {
	groupHash := fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(cuePath)))
	key := fmt.Sprintf("%d_%s_%s", c.Sender().ID, hash, groupHash)

	newGroup := &PendingCueGroup{
		CuePath:        cuePath,
		AlbumPerformer: sheet.Performer,
		Hash:           hash,
		RootTmp:        rootTmp,
	}
	for i := range sheet.Files {
		if len(sheet.Files[i].Tracks) < 2 {
			continue
		}
		newGroup.Files = append(newGroup.Files, &pendingCueGroupFile{
			AudioFile: sheet.Files[i].AudioFile,
			Tracks:    sheet.Files[i].Tracks,
		})
	}

	actual, loaded := pendingCueGroups.LoadOrStore(key, newGroup)
	group := actual.(*PendingCueGroup)

	base := strings.ToLower(filepath.Base(audioPath))
	group.mu.Lock()
	var gf *pendingCueGroupFile
	for _, f := range group.Files {
		if strings.ToLower(filepath.Base(f.AudioFile)) == base {
			gf = f
			break
		}
	}
	if gf == nil {
		group.mu.Unlock()
		log.Printf("[cue] %s: групповой cue %s не содержит секцию для этого файла", audioPath, cuePath)
		return false, nil
	}
	gf.Arrived = true
	gf.DiskPath = audioPath
	gf.FileID = fileID
	gf.Oversized = oversized
	gf.Fallback = fallback
	decided, confirmed := group.Decided, group.Confirmed
	group.mu.Unlock()

	if decided {
		// Решение по группе уже принято (пользователь ответил, пока этот
		// файл ещё качался) — применяем сразу, без нового вопроса.
		return true, applyCueGroupDecision(c, group, gf, confirmed)
	}

	if !loaded {
		// Мы первый физический файл этой группы, что дошёл сюда —
		// показываем ОДНО общее сообщение на весь cue.
		if sendErr := sendCueGroupPrompt(c, group, hash, groupHash); sendErr != nil {
			log.Printf("[cue] %s: не удалось показать групповое меню подтверждения: %v", audioPath, sendErr)
			pendingCueGroups.Delete(key)
			return false, nil
		}
	}
	// Решение ещё не принято — файл просто числится Arrived в группе и
	// будет обработан, когда придёт ответ (см. finishCueGroupDecision).
	return true, nil
}

func sendCueGroupPrompt(c tele.Context, group *PendingCueGroup, hash, groupHash string) error {
	group.mu.Lock()
	var lines []string
	totalTracks := 0
	for _, f := range group.Files {
		lines = append(lines, fmt.Sprintf("• %s — %d треков", filepath.Base(f.AudioFile), len(f.Tracks)))
		totalTracks += len(f.Tracks)
	}
	group.mu.Unlock()

	markup := &tele.ReplyMarkup{}
	markup.Inline(
		markup.Row(markup.Data(fmt.Sprintf("🎼 Нарезать всё (%d треков)", totalTracks), "\fcuegsplit", hash, groupHash)),
		markup.Row(markup.Data("▶️ Отправить всё как есть", "\fcuegskip", hash, groupHash)),
	)
	msgText := fmt.Sprintf("🎼 Найден общий cue-sheet на %d файлов:\n%s\n\nНарезать всё на отдельные треки?", len(group.Files), strings.Join(lines, "\n"))
	sentMsg, err := c.Bot().Send(c.Recipient(), msgText, markup, tele.ModeHTML)
	if err != nil {
		return err
	}
	group.mu.Lock()
	group.PickerMsg = sentMsg
	group.mu.Unlock()
	return nil
}

// handleCueGroupSplitConfirm/handleCueGroupSplitDecline — пользователь
// ответил на общее меню (см. sendCueGroupPrompt). Применяется сразу ко всем
// физическим файлам группы, что УЖЕ докачались (Arrived) — остальные
// подхватят решение сами, дойдя до offerCueGroupSplit позже (Decided уже
// true к этому моменту).
func handleCueGroupSplitConfirm(c tele.Context, hash, groupHash string) error {
	return finishCueGroupDecision(c, hash, groupHash, true)
}

func handleCueGroupSplitDecline(c tele.Context, hash, groupHash string) error {
	return finishCueGroupDecision(c, hash, groupHash, false)
}

func finishCueGroupDecision(c tele.Context, hash, groupHash string, confirmed bool) error {
	key := fmt.Sprintf("%d_%s_%s", c.Sender().ID, hash, groupHash)
	val, ok := pendingCueGroups.Load(key)
	if !ok {
		return c.Respond(&tele.CallbackResponse{Text: "Данные устарели"})
	}
	group := val.(*PendingCueGroup)

	group.mu.Lock()
	if group.Decided {
		group.mu.Unlock()
		return c.Respond(&tele.CallbackResponse{Text: "Уже обработано"})
	}
	group.Decided = true
	group.Confirmed = confirmed
	pickerMsg := group.PickerMsg
	var toProcess []*pendingCueGroupFile
	for _, f := range group.Files {
		if f.Arrived && !f.Done {
			f.Done = true
			toProcess = append(toProcess, f)
		}
	}
	group.mu.Unlock()

	if pickerMsg != nil {
		c.Bot().Delete(pickerMsg)
	}
	if confirmed {
		c.Respond(&tele.CallbackResponse{Text: "Принято"})
	} else {
		c.Respond(&tele.CallbackResponse{Text: "Отправляю как есть"})
	}

	var lastErr error
	for _, f := range toProcess {
		if err := applyCueGroupDecision(c, group, f, confirmed); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// applyCueGroupDecision применяет уже принятое групповое решение к ОДНОМУ
// физическому файлу — либо запускает для него обычный цикл "выбор обложки
// → нарезка" (переиспользуя PendingCueSplit/offerCueCoverSelection, как и
// для одиночного cue), либо отправляет его как есть (с откатом на 7z для
// файлов, пропущенных в обход safePartSize).
func applyCueGroupDecision(c tele.Context, group *PendingCueGroup, f *pendingCueGroupFile, confirmed bool) error {
	if !confirmed {
		if f.Oversized && f.Fallback != nil {
			fbErr := f.Fallback()
			completeAudioTask(group.RootTmp)
			return fbErr
		}
		err := processAudioFileNormally(c, f.DiskPath, group.Hash, group.RootTmp, f.FileID, nil)
		completeAudioTask(group.RootTmp)
		return err
	}

	pcs := &PendingCueSplit{
		AudioPath:      f.DiskPath,
		Tracks:         f.Tracks,
		AlbumPerformer: group.AlbumPerformer,
		Hash:           group.Hash,
		FileID:         f.FileID,
		RootTmp:        group.RootTmp,
		Oversized:      f.Oversized,
		Fallback:       f.Fallback,
	}
	return offerCueCoverSelection(c, pcs)
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
	return processAudioFileNormally(c, pcs.AudioPath, pcs.Hash, pcs.RootTmp, pcs.FileID, nil)
}

// handleCueSplitConfirm — пользователь подтвердил нарезку. Сама нарезка
// откладывается до выбора обложки (см. offerCueCoverSelection) — раньше
// обложка для всего альбома выбиралась автоматически (cueAlbumCover) сразу
// здесь, без участия пользователя.
func handleCueSplitConfirm(c tele.Context, hash, fileHash string) error {
	pcs, err := popPendingCueSplit(c, hash, fileHash)
	if err != nil {
		return c.Respond(&tele.CallbackResponse{Text: "Данные устарели"})
	}
	if pcs.PickerMsg != nil {
		c.Bot().Delete(pcs.PickerMsg)
	}
	c.Respond(&tele.CallbackResponse{Text: "Выбор обложки"})

	return offerCueCoverSelection(c, pcs)
}

// offerCueCoverSelection показывает меню выбора обложки для АЛЬБОМА,
// который сейчас будет нарезан по cue (см. handleCueSplitConfirm) — то же
// меню и те же кнопки (\fcover/\fskip/\fcovup), что и для обычных
// одиночных треков (см. processAudioFileNormally в audio.go), включая
// вшитую в исходный файл обложку как отдельный пункт. Разница только в
// завершении: по выбору управление уходит в finishCueSplit (см. ниже,
// вызывается из handleCoverSelection/handleCoverSkip/handleCustomCoverUpload
// в audio.go через PendingCover.CueSplit), а не в применение обложки
// поштучно к уже готовым файлам.
func offerCueCoverSelection(c tele.Context, pcs *PendingCueSplit) error {
	audioDir := filepath.Dir(pcs.AudioPath)
	fileHash := fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(pcs.AudioPath)))
	key := fmt.Sprintf("%d_%s_%s", c.Sender().ID, pcs.Hash, fileHash)

	images := findImagesInDir(audioDir)

	_, _, _, hasCover, coverData := readAudioInfo(pcs.AudioPath)
	if hasCover && len(coverData) > 0 {
		if embeddedPath, err := saveEmbeddedCoverOption(audioDir, coverData); err == nil {
			images = append([]string{embeddedPath}, images...)
		} else {
			log.Printf("[cue] %s: не удалось сохранить вшитую обложку для меню: %v", pcs.AudioPath, err)
		}
	}

	pc := &PendingCover{
		AudioDir: audioDir,
		Hash:     pcs.Hash,
		Images:   images,
		RootTmp:  pcs.RootTmp,
		CueSplit: pcs,
	}
	pendingCovers.Store(key, pc)

	var err error
	if len(images) > 0 {
		err = offerCoverSelection(c, pcs.Hash, images, fileHash, audioDir)
	} else {
		err = requestCustomCover(c, pcs.Hash, fileHash, audioDir)
	}
	if err != nil {
		log.Printf("[cue] %s: не удалось показать меню выбора обложки: %v", pcs.AudioPath, err)
		pendingCovers.Delete(key)
		completeAudioTask(pcs.RootTmp)
	}
	return err
}

// finishCueSplit готовит выбранную обложку (или её отсутствие) и запускает
// саму нарезку — вызывается из обработчиков выбора обложки в audio.go,
// когда PendingCover.CueSplit != nil.
func finishCueSplit(c tele.Context, pcs *PendingCueSplit, coverPath string) error {
	var coverData []byte
	if coverPath != "" {
		if compressed, err := compressCoverBytes(coverPath); err == nil {
			coverData = compressed
		} else {
			log.Printf("[cue] %s: не удалось подготовить обложку (%v), продолжаю без неё", pcs.AudioPath, err)
		}
	}
	err := performCueSplitWithCover(c, pcs, coverData)
	if err != nil {
		log.Printf("[cue] %s: нарезка завершилась с ошибками: %v", pcs.AudioPath, err)
	}
	return err
}

// performCueSplitWithCover нарезает исходный файл на треки по секции
// cue-sheet и отправляет каждый с уже выбранной обложкой (coverData==nil —
// без обложки). Конец трека — это начало следующего (в пределах ТОЙ ЖЕ
// секции/файла) или конец файла для последнего; INDEX 00 (пре-гэп) уже
// отброшен на этапе разбора.
//
// Юзербот (MTProto) или Bot API выбирается ОДИН раз на весь альбом (а не
// решается заново для каждого трека) — тайминги не зависят от трека, и это
// же убирает частичные состояния вроде "половина треков ушла через
// юзербота, половина как FLAC-документ через Bot API", если пользователь
// окажется непривязан именно в середине нарезки.
func performCueSplitWithCover(c tele.Context, pcs *PendingCueSplit, coverData []byte) error {
	totalDur := time.Duration(getDurationFFprobe(pcs.AudioPath)) * time.Second

	useUserbot := userbot.Ready()
	outExt, codec := ".m4a", "alac"
	if useUserbot {
		outExt, codec = ".flac", "flac"
	}

	var lastErr error
	for i, tr := range pcs.Tracks {
		// Статус-сообщение иначе висело бы неизменным всё время нарезки
		// (может занимать минуты на большой альбом) — см. UpdateAudioProgress
		// и audioTaskEntry.onProgress в tgbot/audio.go.
		UpdateAudioProgress(pcs.RootTmp, fmt.Sprintf("🎼 <b>%s</b>\nНарезка по cue: трек %d из %d", filepath.Base(pcs.AudioPath), i+1, len(pcs.Tracks)))

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

		// Ключ — по исходному файлу И номеру трека внутри него: один и тот
		// же торрент-файл режется на несколько сообщений, у каждого свой
		// закэшированный file_id (см. audioCacheKey в tgbot/audio.go).
		trackCacheKey := fmt.Sprintf("%s#%d", audioCacheKey(pcs.Hash, pcs.FileID), tr.Number)
		if tgfid := db.GetTGFileID(trackCacheKey); tgfid != "" {
			if err := sendCachedAudio(c, tgfid, title, performer); err != nil {
				log.Printf("[cue] %s: трек %d не отправлен из кэша: %v", pcs.AudioPath, tr.Number, err)
				lastErr = err
			} else {
				log.Printf("[cue] %s: трек %d отправлен из кэша Telegram (file_id)", pcs.AudioPath, tr.Number)
			}
			continue
		}

		durSecs := int((end - tr.Start).Seconds())

		outPath, err := cutCueTrack(pcs.AudioPath, tr.Start, end, tr.Number, outExt, codec)
		if err != nil {
			log.Printf("[cue] %s: не удалось нарезать трек %d (%v–%v): %v", pcs.AudioPath, tr.Number, tr.Start, end, err)
			lastErr = err
			continue
		}

		if useUserbot {
			msgID, chatID, sendErr := userbot.SendToRelay(context.Background(), outPath, title, performer, durSecs, coverData)
			var sent *tele.Message
			if sendErr == nil {
				sent, sendErr = c.Bot().Copy(c.Recipient(), tele.StoredMessage{MessageID: strconv.Itoa(msgID), ChatID: chatID})
			}
			if sendErr == nil {
				if sent != nil && sent.Audio != nil && sent.Audio.FileID != "" {
					db.SaveTGFileID(trackCacheKey, sent.Audio.FileID)
				}
				continue
			}

			// Юзербот/релей подвели (например, при нескольких одновременных
			// загрузках через один и тот же MTProto-коннекшн — см. broken
			// pipe в логах) — раньше трек тут просто пропускался и терялся
			// молча (пользователь недосчитывался треков в альбоме). Теперь
			// откатываемся на Bot API: перерезаем этот ЖЕ трек в M4A (Bot
			// API не принимает произвольный FLAC для sendAudio) и шлём как
			// обычно. FLAC-версия (outPath) не удаляется явно — уйдёт вместе
			// со всей tmpDir задачи, как и остальные промежуточные файлы.
			log.Printf("[cue] %s: трек %d не отправлен через userbot (%v), откатываюсь на Bot API (M4A)", pcs.AudioPath, tr.Number, sendErr)
			m4aPath, cutErr := cutCueTrack(pcs.AudioPath, tr.Start, end, tr.Number, ".m4a", "alac")
			if cutErr != nil {
				log.Printf("[cue] %s: повторная нарезка трека %d в M4A для отката не удалась: %v", pcs.AudioPath, tr.Number, cutErr)
				lastErr = sendErr
				continue
			}
			if err := sendAudio(c, m4aPath, performer, title, durSecs, coverData, trackCacheKey); err != nil {
				log.Printf("[cue] %s: трек %d не отправлен через Bot API после отката: %v", pcs.AudioPath, tr.Number, err)
				lastErr = err
			}
			continue
		}

		if err := sendAudio(c, outPath, performer, title, durSecs, coverData, trackCacheKey); err != nil {
			log.Printf("[cue] %s: трек %d не отправлен: %v", pcs.AudioPath, tr.Number, err)
			lastErr = err
		}
	}
	return lastErr
}

// sendCachedAudio пересылает уже когда-то отправленный трек по
// закэшированному Telegram file_id — без повторного скачивания/нарезки/
// заливки (см. db.SaveTGFileID/GetTGFileID и trackCacheKey в
// performCueSplit).
func sendCachedAudio(c tele.Context, fileID, title, performer string) error {
	audio := &tele.Audio{File: tele.File{FileID: fileID}, Title: title, Performer: performer}
	_, err := c.Bot().Send(c.Recipient(), audio)
	return err
}

// cutCueTrack вырезает [start, end) из srcPath и перекодирует в формат под
// целевой канал доставки: FLAC (без потерь, для юзербота/MTProto) либо
// ALAC/M4A (для Bot API — sendAudio принимает только .mp3/.m4a). Оба —
// lossless-кодеки, разница только в контейнере/совместимости с получателем.
// -ss ДО -i даёt быстрый seek по входному файлу; -to при этом трактуется как
// абсолютная позиция в исходном таймлайне (а не относительно точки seek),
// что и нужно — Start/End уже абсолютные тайминги внутри СВОЕГО файла из
// cue-sheet. trackNum входит в имя выходного файла, а не в порядковый номер
// внутри секции, поэтому имена разных файлов одной папки (см.
// CueFileSection) не конфликтуют между собой только благодаря тому, что
// резка каждого файла идёт в его же каталоге — collision тут невозможен,
// т.к. номера треков в cue уникальны по всему документу (см. сквозную
// нумерацию TRACK NN в примере ICE MC).
func cutCueTrack(srcPath string, start, end time.Duration, trackNum int, outExt, codec string) (string, error) {
	outPath := filepath.Join(filepath.Dir(srcPath), fmt.Sprintf("cue_track_%02d%s", trackNum, outExt))
	args := []string{
		"-ss", formatFFmpegTime(start),
		"-to", formatFFmpegTime(end),
		"-i", srcPath,
		"-map", "0:a",
		"-c:a", codec,
	}
	if codec == "alac" {
		// movflags актуален только для MOV/MP4-контейнера (.m4a); для
		// нативного FLAC-контейнера ffmpeg эту опцию не понимает.
		args = append(args, "-movflags", "+faststart")
	}
	args = append(args, "-vn", outPath)

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
