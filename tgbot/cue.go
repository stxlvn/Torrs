package tgbot

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"html"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tele "gopkg.in/telebot.v4"

	"torrsru/db"
	"torrsru/tgbot/torr"
	"torrsru/tgbot/userbot"
)

const (
	userCueName           = ".torrs_user.cue"
	maxCueSize            = 1 << 20
	cuelessMinDuration    = 10 * time.Minute
	cuelessMaxFolderAudio = 4
	cuePreviewLines       = 12
	oversizedLimit        = 1_900_000_000
)

// cueFileCtx — один аудиофайл задачи, ожидающий решения по cue.
type cueFileCtx struct {
	AudioPath string
	Hash      string
	RootTmp   string
	FileID    int
	Oversized bool
	Fallback  func() error
}

func crcHex(s string) string { return fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(s))) }

func (f cueFileCtx) fileHash() string { return crcHex(f.AudioPath) }
func (f cueFileCtx) dir() string      { return filepath.Dir(f.AudioPath) }

func userKey(c tele.Context, hash, h string) string {
	return fmt.Sprintf("%d_%s_%s", c.Sender().ID, hash, h)
}

// CueTrackMeta — PERFORMER/TITLE единственного трека секции (релиз уже
// нарезан, cue служит только источником тегов).
type CueTrackMeta struct {
	Artist string
	Title  string
}

type PendingCueSplit struct {
	cueFileCtx
	Sheet     *CueSheet
	Cuts      []CueCut
	Source    string
	PickerMsg *tele.Message
	group     *PendingCueGroup
}

type pendingCuelessFile struct {
	cueFileCtx
	PickerMsg *tele.Message
}

type pendingCueGroupFile struct {
	Rel     string
	Section int
	Arrived bool
	Done    bool
	ctx     cueFileCtx
}

type PendingCueGroup struct {
	CuePath   string
	Sheet     *CueSheet
	Source    string
	Dir       string
	Files     []*pendingCueGroupFile
	PickerMsg *tele.Message
	Decided   bool
	Confirmed bool

	coverAsked   bool
	coverChosen  bool
	coverData    []byte
	coverWaiting []*PendingCueSplit

	mu sync.Mutex
}

type cueDirInfo struct{ Dir, RootTmp, Hash string }

type cueUploadTarget struct{ Hash, DirHash string }

var (
	pendingCueSplits    sync.Map // user_hash_fileHash -> *PendingCueSplit
	pendingCuelessFiles sync.Map // user_hash_fileHash -> *pendingCuelessFile
	pendingCueGroups    sync.Map // user_hash_crc(cuePath) -> *PendingCueGroup
	cueDirs             sync.Map // user_hash_dirHash -> cueDirInfo
	cueExpect           sync.Map // userID -> cueUploadTarget
)

func registerCueDir(c tele.Context, fc cueFileCtx) string {
	dh := crcHex(fc.dir())
	cueDirs.Store(userKey(c, fc.Hash, dh), cueDirInfo{Dir: fc.dir(), RootTmp: fc.RootTmp, Hash: fc.Hash})
	return dh
}

// ------------------------------------------------------ поиск cue для файла

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func ancestorDirs(dir, rootTmp string) []string {
	var dirs []string
	for d := dir; ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if d == rootTmp || !strings.HasPrefix(d, rootTmp) || d == filepath.Dir(d) {
			break
		}
	}
	return dirs
}

// cueCandidates — cue-файлы, которые могут описывать audioPath: загруженный
// пользователем (он один и перекрывает остальные), затем .cue из папки файла
// и родительских папок; одноимённые с аудиофайлом — первыми.
func cueCandidates(audioPath, rootTmp string) []string {
	dirs := ancestorDirs(filepath.Dir(audioPath), rootTmp)
	for _, d := range dirs {
		if p := filepath.Join(d, userCueName); fileExists(p) {
			return []string{p}
		}
	}
	base := strings.ToLower(filepath.Base(audioPath))
	stem := strings.TrimSuffix(base, strings.ToLower(filepath.Ext(base)))
	var out []string
	for _, d := range dirs {
		entries, _ := os.ReadDir(d)
		var first, rest []string
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() || n == userCueName || !strings.EqualFold(filepath.Ext(n), ".cue") {
				continue
			}
			cs := strings.ToLower(strings.TrimSuffix(n, filepath.Ext(n)))
			if cs == stem || cs == base {
				first = append(first, filepath.Join(d, n))
			} else {
				rest = append(rest, filepath.Join(d, n))
			}
		}
		sort.Slice(rest, func(i, j int) bool { return naturalLess(rest[i], rest[j]) })
		out = append(append(out, first...), rest...)
	}
	return out
}

// torrentAudioRels — аудиофайлы раздачи внутри baseDir (пути относительно
// baseDir через "/"), по списку файлов торрента, а не диска: часть файлов
// может быть ещё не скачана.
func torrentAudioRels(hash, rootTmp, baseDir string, recursive bool) []string {
	var rels []string
	add := func(full string) {
		rel, err := filepath.Rel(baseDir, full)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return
		}
		rel = filepath.ToSlash(rel)
		if !recursive && strings.Contains(rel, "/") {
			return
		}
		rels = append(rels, rel)
	}
	if ti, err := torr.GetTorrentInfo(hash); err == nil && ti != nil && len(ti.FileStats) > 0 {
		for _, f := range ti.FileStats {
			if torr.IsAudioExt(f.Path) {
				add(filepath.Join(rootTmp, strings.TrimPrefix(f.Path, "/")))
			}
		}
		return rels
	}
	filepath.WalkDir(baseDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != baseDir && !recursive {
				return fs.SkipDir
			}
			return nil
		}
		if torr.IsAudioExt(p) {
			add(p)
		}
		return nil
	})
	return rels
}

// cueHints — строки раздачи в UTF-8 (имена файлов и папок, название
// торрента), по которым выбирается кодировка cue.
func cueHints(hash, rootTmp, cuePath string, rels []string) []string {
	hints := append([]string{}, rels...)
	if rel, err := filepath.Rel(rootTmp, cuePath); err == nil {
		hints = append(hints, strings.Split(filepath.ToSlash(rel), "/")...)
	}
	if ti, err := torr.GetTorrentInfo(hash); err == nil && ti != nil {
		hints = append(hints, ti.Title)
	}
	return hints
}

type cueMatch struct {
	Sheet   *CueSheet
	Source  string
	CuePath string
	CueDir  string
	Section int
	Rels    map[int]string
}

func (m *cueMatch) qualifyingSections() []int {
	var out []int
	for idx, rel := range m.Rels {
		if rel != "" && len(m.Sheet.Files[idx].Tracks) >= 2 {
			out = append(out, idx)
		}
	}
	sort.Ints(out)
	return out
}

func relFrom(dir, p string) string {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return filepath.Base(p)
	}
	return filepath.ToSlash(rel)
}

func cueSourceLabel(cuePath string, sheet *CueSheet) string {
	name := filepath.Base(cuePath)
	if name == userCueName {
		name = "ваш cue"
	}
	return fmt.Sprintf("%s, кодировка %s", name, sheet.Encoding)
}

func findCueForFile(fc cueFileCtx) (*cueMatch, *CueTrackMeta) {
	var meta *CueTrackMeta
	for _, cuePath := range cueCandidates(fc.AudioPath, fc.RootTmp) {
		data, err := os.ReadFile(cuePath)
		if err != nil || len(data) > maxCueSize {
			log.Printf("[cue] %s: пропуск %s: %v (size=%d)", fc.AudioPath, cuePath, err, len(data))
			continue
		}
		cueDir := filepath.Dir(cuePath)
		rels := torrentAudioRels(fc.Hash, fc.RootTmp, cueDir, true)
		sheet, err := parseCueSheet(data, cueHints(fc.Hash, fc.RootTmp, cuePath, rels)...)
		if err != nil {
			log.Printf("[cue] %s: не удалось разобрать %s: %v", fc.AudioPath, cuePath, err)
			continue
		}
		matched := matchCueSections(sheet, rels)
		myRel := relFrom(cueDir, fc.AudioPath)
		section := -1
		for idx, rel := range matched {
			if rel == myRel {
				section = idx
			}
		}
		log.Printf("[cue] %s: %s (%s): FILE-секций=%d, сопоставлено=%d, секция файла=%d", fc.AudioPath, cuePath, sheet.Encoding, len(sheet.Files), len(matched), section)
		if section < 0 {
			continue
		}
		tracks := sheet.Files[section].Tracks
		if len(tracks) < 2 {
			if len(tracks) == 1 && meta == nil {
				meta = &CueTrackMeta{Artist: firstNonEmpty(tracks[0].Performer, sheet.Performer), Title: tracks[0].Title}
			}
			continue
		}
		return &cueMatch{Sheet: sheet, Source: cueSourceLabel(cuePath, sheet), CuePath: cuePath, CueDir: cueDir, Section: section, Rels: matched}, nil
	}
	if meta != nil {
		return nil, meta
	}

	p, err := probeAudio(fc.AudioPath)
	if err != nil {
		return nil, nil
	}
	sheet, source := embeddedCue(p)
	if sheet == nil {
		return nil, nil
	}
	section := 0
	if len(sheet.Files) > 1 {
		m := matchCueSections(sheet, []string{filepath.Base(fc.AudioPath)})
		section = -1
		for idx := range m {
			section = idx
		}
		if section < 0 {
			for idx := range sheet.Files {
				if section < 0 || len(sheet.Files[idx].Tracks) > len(sheet.Files[section].Tracks) {
					section = idx
				}
			}
		}
	}
	if len(sheet.Files[section].Tracks) < 2 {
		return nil, nil
	}
	log.Printf("[cue] %s: найден %s, треков=%d", fc.AudioPath, source, len(sheet.Files[section].Tracks))
	return &cueMatch{Sheet: sheet, Source: source, Section: section, Rels: map[int]string{section: filepath.Base(fc.AudioPath)}}, nil
}

// processWithCue — точка входа аудиофайла: нарезка по cue, предложение
// прислать свой cue для образа без него, или обычная отправка.
// auto=true — cue прислал сам пользователь, повторно не переспрашиваем.
func processWithCue(c tele.Context, fc cueFileCtx, auto bool) error {
	m, meta := findCueForFile(fc)
	if m != nil {
		auto = auto || filepath.Base(m.CuePath) == userCueName
		if m.CuePath != "" && len(m.qualifyingSections()) > 1 {
			return offerCueGroupSplit(c, fc, m, auto)
		}
		return offerCueSplit(c, fc, m, auto)
	}
	if meta == nil {
		if handled, err := offerCueless(c, fc); handled {
			return err
		}
	}
	return proceedAsIs(c, fc, meta)
}

func proceedAsIs(c tele.Context, fc cueFileCtx, meta *CueTrackMeta) error {
	if fc.Oversized {
		if small, ok := shrinkOversized(fc.AudioPath); ok {
			os.Remove(fc.AudioPath)
			return processAudioFileWithKey(c, small, fc.Hash, fc.RootTmp, audioCacheKey(fc.Hash, fc.FileID), meta)
		}
		if fc.Fallback == nil {
			completeAudioTask(fc.RootTmp)
			return errors.New("файл превышает 1.9 ГБ, разбиение не настроено")
		}
		err := fc.Fallback()
		completeAudioTask(fc.RootTmp)
		return err
	}
	return processAudioFileNormally(c, fc.AudioPath, fc.Hash, fc.RootTmp, fc.FileID, meta)
}

// shrinkOversized пробует уложить файл больше лимита в обычную отправку,
// сжав его без потерь (DSD/WAV/AIFF и т.п. -> FLAC или ALAC).
func shrinkOversized(src string) (string, bool) {
	ext := strings.ToLower(filepath.Ext(src))
	if ext == ".flac" || ext == ".mp3" || ext == ".m4a" || ext == ".ogg" {
		return "", false
	}
	out, ok := convertForTelegram(src)
	if !ok {
		return "", false
	}
	st, err := os.Stat(out)
	if err != nil || st.Size() >= oversizedLimit {
		os.Remove(out)
		log.Printf("[audio] %s: после сжатия всё ещё больше лимита, архивирую", src)
		return "", false
	}
	log.Printf("[audio] %s: сжат до %d байт, отправляю как трек", src, st.Size())
	return out, true
}

func skipLabel(oversized bool, all bool) string {
	switch {
	case oversized:
		return "📦 Отправить архивом (7z)"
	case all:
		return "▶️ Отправить всё как есть"
	}
	return "▶️ Отправить как есть"
}

func cuePreview(sheet *CueSheet, cuts []CueCut, max int) string {
	var lines []string
	for i, cut := range cuts {
		if i == max {
			lines = append(lines, fmt.Sprintf("… и ещё %d", len(cuts)-max))
			break
		}
		line := fmt.Sprintf("%02d. %s", cut.Number, cut.Title)
		if cut.Performer != "" && cut.Performer != sheet.Performer {
			line += " — " + cut.Performer
		}
		lines = append(lines, html.EscapeString(line))
	}
	return strings.Join(lines, "\n")
}

func cueAlbumLine(sheet *CueSheet) string {
	var parts []string
	if sheet.Performer != "" {
		parts = append(parts, sheet.Performer)
	}
	if sheet.Title != "" {
		parts = append(parts, sheet.Title)
	}
	if len(parts) == 0 {
		return ""
	}
	s := strings.Join(parts, " — ")
	if sheet.Date != "" {
		s += " (" + sheet.Date + ")"
	}
	return "💿 " + html.EscapeString(s) + "\n"
}

func formatDuration(d time.Duration) string {
	s := int(d.Seconds())
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// ---------------------------------------------------- одиночный cue-файл

func offerCueSplit(c tele.Context, fc cueFileCtx, m *cueMatch, auto bool) error {
	sec := &m.Sheet.Files[m.Section]
	pcs := &PendingCueSplit{cueFileCtx: fc, Sheet: m.Sheet, Cuts: m.Sheet.Cuts(sec), Source: m.Source}
	if auto {
		return offerCueCoverSelection(c, pcs)
	}

	key := userKey(c, fc.Hash, fc.fileHash())
	pendingCueSplits.Store(key, pcs)
	dh := registerCueDir(c, fc)

	markup := &tele.ReplyMarkup{}
	markup.Inline(
		markup.Row(markup.Data(fmt.Sprintf("🎼 Нарезать на %d треков", len(pcs.Cuts)), "\fcuesplit", fc.Hash, fc.fileHash())),
		markup.Row(markup.Data("📄 Загрузить свой CUE", "\fcueup", fc.Hash, dh)),
		markup.Row(markup.Data(skipLabel(fc.Oversized, false), "\fcueskip", fc.Hash, fc.fileHash())),
	)
	text := fmt.Sprintf("🎼 <b>%s</b>\n%sНайден cue-sheet (%s), треков: %d\n\n%s\n\nНарезать на отдельные треки?",
		html.EscapeString(filepath.Base(fc.AudioPath)), cueAlbumLine(m.Sheet), html.EscapeString(m.Source), len(pcs.Cuts),
		cuePreview(m.Sheet, pcs.Cuts, cuePreviewLines))
	msg, err := c.Bot().Send(c.Recipient(), text, markup, tele.ModeHTML)
	if err != nil {
		log.Printf("[cue] %s: не удалось показать меню нарезки: %v", fc.AudioPath, err)
		pendingCueSplits.Delete(key)
		return proceedAsIs(c, fc, nil)
	}
	pcs.PickerMsg = msg
	return nil
}

func popPendingCueSplit(c tele.Context, hash, fileHash string) (*PendingCueSplit, error) {
	val, ok := pendingCueSplits.LoadAndDelete(userKey(c, hash, fileHash))
	if !ok {
		return nil, errors.New("данные устарели")
	}
	return val.(*PendingCueSplit), nil
}

func handleCueSplitDecline(c tele.Context, hash, fileHash string) error {
	pcs, err := popPendingCueSplit(c, hash, fileHash)
	if err != nil {
		return c.Respond(&tele.CallbackResponse{Text: "Данные устарели"})
	}
	if pcs.PickerMsg != nil {
		c.Bot().Delete(pcs.PickerMsg)
	}
	c.Respond(&tele.CallbackResponse{Text: "Отправляю без нарезки"})
	return proceedAsIs(c, pcs.cueFileCtx, nil)
}

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

// ------------------------------------------ один cue на несколько файлов

func offerCueGroupSplit(c tele.Context, fc cueFileCtx, m *cueMatch, auto bool) error {
	groupHash := crcHex(m.CuePath)
	key := userKey(c, fc.Hash, groupHash)

	newGroup := &PendingCueGroup{CuePath: m.CuePath, Sheet: m.Sheet, Source: m.Source, Dir: fc.dir()}
	for _, idx := range m.qualifyingSections() {
		newGroup.Files = append(newGroup.Files, &pendingCueGroupFile{Rel: m.Rels[idx], Section: idx})
	}
	if auto {
		newGroup.Decided, newGroup.Confirmed = true, true
	}
	actual, loaded := pendingCueGroups.LoadOrStore(key, newGroup)
	group := actual.(*PendingCueGroup)

	myRel := relFrom(m.CueDir, fc.AudioPath)
	group.mu.Lock()
	var gf *pendingCueGroupFile
	for _, f := range group.Files {
		if f.Rel == myRel {
			gf = f
		}
	}
	if gf == nil || gf.Done {
		group.mu.Unlock()
		log.Printf("[cue] %s: нет свободной секции в групповом cue %s", fc.AudioPath, m.CuePath)
		return proceedAsIs(c, fc, nil)
	}
	gf.Arrived, gf.ctx = true, fc
	decided, confirmed := group.Decided, group.Confirmed
	if decided {
		gf.Done = true
	}
	group.mu.Unlock()

	if decided {
		return applyCueGroupDecision(c, group, gf, confirmed)
	}
	if loaded {
		return nil
	}
	if err := sendCueGroupPrompt(c, group, fc, groupHash); err != nil {
		log.Printf("[cue] %s: не удалось показать групповое меню: %v", fc.AudioPath, err)
		pendingCueGroups.Delete(key)
		group.mu.Lock()
		gf.Done = true
		group.mu.Unlock()
		return proceedAsIs(c, fc, nil)
	}
	return nil
}

func sendCueGroupPrompt(c tele.Context, group *PendingCueGroup, fc cueFileCtx, groupHash string) error {
	group.mu.Lock()
	var lines []string
	total := 0
	for _, f := range group.Files {
		cuts := group.Sheet.Cuts(&group.Sheet.Files[f.Section])
		total += len(cuts)
		lines = append(lines, fmt.Sprintf("• %s — треки %02d–%02d", html.EscapeString(f.Rel), cuts[0].Number, cuts[len(cuts)-1].Number))
	}
	group.mu.Unlock()

	dh := registerCueDir(c, fc)
	markup := &tele.ReplyMarkup{}
	markup.Inline(
		markup.Row(markup.Data(fmt.Sprintf("🎼 Нарезать всё (%d треков)", total), "\fcuegsplit", fc.Hash, groupHash)),
		markup.Row(markup.Data("📄 Загрузить свой CUE", "\fcueup", fc.Hash, dh)),
		markup.Row(markup.Data(skipLabel(false, true), "\fcuegskip", fc.Hash, groupHash)),
	)
	text := fmt.Sprintf("🎼 %sОдин cue-sheet (%s) на %d файлов:\n%s\n\nНарезать всё на отдельные треки?",
		cueAlbumLine(group.Sheet), html.EscapeString(group.Source), len(group.Files), strings.Join(lines, "\n"))
	msg, err := c.Bot().Send(c.Recipient(), text, markup, tele.ModeHTML)
	if err != nil {
		return err
	}
	group.mu.Lock()
	group.PickerMsg = msg
	group.mu.Unlock()
	return nil
}

func handleCueGroupSplitConfirm(c tele.Context, hash, groupHash string) error {
	return finishCueGroupDecision(c, hash, groupHash, true)
}

func handleCueGroupSplitDecline(c tele.Context, hash, groupHash string) error {
	return finishCueGroupDecision(c, hash, groupHash, false)
}

func finishCueGroupDecision(c tele.Context, hash, groupHash string, confirmed bool) error {
	val, ok := pendingCueGroups.Load(userKey(c, hash, groupHash))
	if !ok {
		return c.Respond(&tele.CallbackResponse{Text: "Данные устарели"})
	}
	group := val.(*PendingCueGroup)

	group.mu.Lock()
	if group.Decided {
		group.mu.Unlock()
		return c.Respond(&tele.CallbackResponse{Text: "Уже обработано"})
	}
	group.Decided, group.Confirmed = true, confirmed
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
		c.Respond(&tele.CallbackResponse{Text: "Отправляю без нарезки"})
	}
	var lastErr error
	for _, f := range toProcess {
		if err := applyCueGroupDecision(c, group, f, confirmed); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func applyCueGroupDecision(c tele.Context, group *PendingCueGroup, f *pendingCueGroupFile, confirmed bool) error {
	if !confirmed {
		return proceedAsIs(c, f.ctx, nil)
	}
	pcs := &PendingCueSplit{cueFileCtx: f.ctx, Sheet: group.Sheet, Cuts: group.Sheet.Cuts(&group.Sheet.Files[f.Section]), Source: group.Source, group: group}
	group.mu.Lock()
	switch {
	case group.coverChosen:
		cover := group.coverData
		group.mu.Unlock()
		err := performCueSplitWithCover(c, pcs, cover)
		completeAudioTask(pcs.RootTmp)
		return err
	case group.coverAsked:
		group.coverWaiting = append(group.coverWaiting, pcs)
		group.mu.Unlock()
		return nil
	}
	group.coverAsked = true
	group.mu.Unlock()
	return offerCueCoverSelection(c, pcs)
}

// ------------------------------------------------ образ без cue-sheet

// offerCueless предлагает прислать свой cue, если файл похож на альбом
// одним файлом: длинный и лежит не в папке с уже нарезанными треками.
func offerCueless(c tele.Context, fc cueFileCtx) (bool, error) {
	var dur time.Duration
	if p, err := probeAudio(fc.AudioPath); err == nil {
		dur = p.Duration
	}
	if !fc.Oversized && dur < cuelessMinDuration {
		return false, nil
	}
	if n := len(torrentAudioRels(fc.Hash, fc.RootTmp, fc.dir(), false)); n > cuelessMaxFolderAudio {
		return false, nil
	}

	key := userKey(c, fc.Hash, fc.fileHash())
	pf := &pendingCuelessFile{cueFileCtx: fc}
	pendingCuelessFiles.Store(key, pf)
	dh := registerCueDir(c, fc)

	markup := &tele.ReplyMarkup{}
	markup.Inline(
		markup.Row(markup.Data("📄 Загрузить свой CUE", "\fcueup", fc.Hash, dh)),
		markup.Row(markup.Data(skipLabel(fc.Oversized, false), "\fcuenone", fc.Hash, fc.fileHash())),
	)
	durText := ""
	if dur > 0 {
		durText = " (" + formatDuration(dur) + ")"
	}
	text := fmt.Sprintf("💿 <b>%s</b>%s\nПохоже на альбом одним файлом, но cue-sheet не найден.\n\nНажмите «Загрузить свой CUE» и пришлите .cue файлом или текстом — бот нарежет альбом по нему.",
		html.EscapeString(filepath.Base(fc.AudioPath)), durText)
	msg, err := c.Bot().Send(c.Recipient(), text, markup, tele.ModeHTML)
	if err != nil {
		pendingCuelessFiles.Delete(key)
		log.Printf("[cue] %s: не удалось предложить загрузку cue: %v", fc.AudioPath, err)
		return false, nil
	}
	pf.PickerMsg = msg
	return true, nil
}

func handleCuelessSkip(c tele.Context, hash, fileHash string) error {
	val, ok := pendingCuelessFiles.LoadAndDelete(userKey(c, hash, fileHash))
	if !ok {
		return c.Respond(&tele.CallbackResponse{Text: "Данные устарели"})
	}
	pf := val.(*pendingCuelessFile)
	if pf.PickerMsg != nil {
		c.Bot().Delete(pf.PickerMsg)
	}
	c.Respond(&tele.CallbackResponse{Text: "Отправляю без нарезки"})
	return proceedAsIs(c, pf.cueFileCtx, nil)
}

// ------------------------------------------------ свой cue от пользователя

func handleCueUploadRequest(c tele.Context, hash, dirHash string) error {
	if _, ok := cueDirs.Load(userKey(c, hash, dirHash)); !ok {
		return c.Respond(&tele.CallbackResponse{Text: "Данные устарели"})
	}
	cueExpect.Store(c.Sender().ID, cueUploadTarget{Hash: hash, DirHash: dirHash})
	c.Respond(&tele.CallbackResponse{Text: "Пришлите .cue"})
	_, err := c.Bot().Send(c.Recipient(), "📄 Пришлите cue-sheet файлом .cue или просто текстом сообщения.\nКодировка любая (UTF-8, Windows-1251, Shift-JIS, GBK, Big5, EUC-KR и др.) — определится автоматически.")
	return err
}

func looksLikeCueText(s string) bool {
	u := strings.ToUpper(s)
	return strings.Contains(u, "TRACK") && strings.Contains(u, "INDEX")
}

func handleCueUpload(c tele.Context, target cueUploadTarget, data []byte) error {
	val, ok := cueDirs.Load(userKey(c, target.Hash, target.DirHash))
	if !ok {
		return c.Send("Данные устарели — эта загрузка уже завершена.")
	}
	info := val.(cueDirInfo)
	rearm := func() { cueExpect.Store(c.Sender().ID, target) }

	cuePath := filepath.Join(info.Dir, userCueName)
	rels := torrentAudioRels(info.Hash, info.RootTmp, info.Dir, true)
	sheet, err := parseCueSheet(data, cueHints(info.Hash, info.RootTmp, cuePath, rels)...)
	if err != nil {
		rearm()
		return c.Send(fmt.Sprintf("❌ Не удалось разобрать CUE: %s\nПришлите исправленный cue.", html.EscapeString(err.Error())), tele.ModeHTML)
	}
	matched := matchCueSections(sheet, rels)
	if len(matched) == 0 {
		rearm()
		var cueFiles []string
		for _, f := range sheet.Files {
			if len(f.Tracks) > 0 {
				cueFiles = append(cueFiles, "• "+html.EscapeString(f.AudioFile))
			}
		}
		var have []string
		for _, r := range rels {
			have = append(have, "• "+html.EscapeString(r))
		}
		return c.Send(fmt.Sprintf("❌ Этот CUE не подходит к файлам папки.\n\nВ cue:\n%s\n\nВ папке:\n%s\n\nПришлите другой cue.",
			strings.Join(cueFiles, "\n"), strings.Join(have, "\n")), tele.ModeHTML)
	}
	if err := os.WriteFile(cuePath, data, 0o644); err != nil {
		return c.Send("❌ Не удалось сохранить cue: " + err.Error())
	}

	var idxs []int
	for idx := range matched {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	var cuts []CueCut
	for _, idx := range idxs {
		cuts = append(cuts, sheet.Cuts(&sheet.Files[idx])...)
	}
	c.Send(fmt.Sprintf("✅ CUE принят (кодировка %s): файлов %d, треков %d\n%s\n%s",
		html.EscapeString(sheet.Encoding), len(idxs), len(cuts), cueAlbumLine(sheet), cuePreview(sheet, cuts, 20)), tele.ModeHTML)
	log.Printf("[cue] пользователь %d прислал cue для %s: кодировка=%s файлов=%d треков=%d", c.Sender().ID, info.Dir, sheet.Encoding, len(idxs), len(cuts))

	waiting := collectWaitingCueFiles(c, info)
	if len(waiting) == 0 {
		return c.Send("Файлы этой папки уже обработаны — cue применится к тем, что ещё скачиваются.")
	}
	var lastErr error
	for _, fc := range waiting {
		if err := processWithCue(c, fc, true); err != nil {
			log.Printf("[cue] %s: обработка с пользовательским cue: %v", fc.AudioPath, err)
			lastErr = err
		}
	}
	return lastErr
}

// collectWaitingCueFiles снимает с ожидания все файлы папки, по которым
// ещё не принято решение по cue, и убирает их меню.
func collectWaitingCueFiles(c tele.Context, info cueDirInfo) []cueFileCtx {
	prefix := fmt.Sprintf("%d_%s_", c.Sender().ID, info.Hash)
	var out []cueFileCtx
	take := func(m *sync.Map, pick func(v any) (cueFileCtx, *tele.Message)) {
		m.Range(func(k, v any) bool {
			if !strings.HasPrefix(k.(string), prefix) {
				return true
			}
			fc, msg := pick(v)
			if fc.dir() != info.Dir {
				return true
			}
			if _, ok := m.LoadAndDelete(k); ok {
				if msg != nil {
					c.Bot().Delete(msg)
				}
				out = append(out, fc)
			}
			return true
		})
	}
	take(&pendingCueSplits, func(v any) (cueFileCtx, *tele.Message) {
		p := v.(*PendingCueSplit)
		return p.cueFileCtx, p.PickerMsg
	})
	take(&pendingCuelessFiles, func(v any) (cueFileCtx, *tele.Message) {
		p := v.(*pendingCuelessFile)
		return p.cueFileCtx, p.PickerMsg
	})
	pendingCueGroups.Range(func(k, v any) bool {
		group := v.(*PendingCueGroup)
		if !strings.HasPrefix(k.(string), prefix) || group.Dir != info.Dir {
			return true
		}
		group.mu.Lock()
		if group.Decided {
			group.mu.Unlock()
			return true
		}
		group.Decided = true
		msg := group.PickerMsg
		for _, f := range group.Files {
			if f.Arrived && !f.Done {
				f.Done = true
				out = append(out, f.ctx)
			}
		}
		group.mu.Unlock()
		pendingCueGroups.Delete(k)
		if msg != nil {
			c.Bot().Delete(msg)
		}
		return true
	})
	return out
}

// ------------------------------------------------------ обложка и нарезка

func offerCueCoverSelection(c tele.Context, pcs *PendingCueSplit) error {
	audioDir := filepath.Dir(pcs.AudioPath)
	fileHash := pcs.fileHash()
	key := userKey(c, pcs.Hash, fileHash)

	images := findImagesInDir(audioDir)
	_, _, _, hasCover, coverData := readAudioInfo(pcs.AudioPath)
	if hasCover && len(coverData) > 0 {
		if embeddedPath, err := saveEmbeddedCoverOption(audioDir, coverData); err == nil {
			images = append([]string{embeddedPath}, images...)
		} else {
			log.Printf("[cue] %s: не удалось сохранить вшитую обложку для меню: %v", pcs.AudioPath, err)
		}
	}

	pc := &PendingCover{AudioDir: audioDir, Hash: pcs.Hash, Images: images, RootTmp: pcs.RootTmp, CueSplit: pcs}
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
		if g := pcs.group; g != nil {
			g.mu.Lock()
			g.coverChosen = true
			waiting := g.coverWaiting
			g.coverWaiting = nil
			g.mu.Unlock()
			for _, w := range waiting {
				performCueSplitWithCover(c, w, nil)
				completeAudioTask(w.RootTmp)
			}
		}
	}
	return err
}

func finishCueSplit(c tele.Context, pcs *PendingCueSplit, coverPath string) error {
	var coverData []byte
	if coverPath != "" {
		if compressed, err := compressCoverBytes(coverPath); err == nil {
			coverData = compressed
		} else {
			log.Printf("[cue] %s: не удалось подготовить обложку (%v), продолжаю без неё", pcs.AudioPath, err)
		}
	}
	var waiting []*PendingCueSplit
	if g := pcs.group; g != nil {
		g.mu.Lock()
		g.coverChosen, g.coverData = true, coverData
		waiting, g.coverWaiting = g.coverWaiting, nil
		g.mu.Unlock()
	}
	err := performCueSplitWithCover(c, pcs, coverData)
	if err != nil {
		log.Printf("[cue] %s: нарезка завершилась с ошибками: %v", pcs.AudioPath, err)
	}
	for _, w := range waiting {
		if werr := performCueSplitWithCover(c, w, coverData); werr != nil {
			log.Printf("[cue] %s: нарезка завершилась с ошибками: %v", w.AudioPath, werr)
			err = werr
		}
		completeAudioTask(w.RootTmp)
	}
	return err
}

func cueTags(sheet *CueSheet, cut CueCut) map[string]string {
	tags := map[string]string{
		"title":        cut.Title,
		"artist":       cut.Performer,
		"album":        sheet.Title,
		"album_artist": sheet.Performer,
		"date":         sheet.Date,
		"genre":        sheet.Genre,
		"composer":     firstNonEmpty(cut.Songwriter, sheet.Composer),
		"ISRC":         cut.ISRC,
	}
	if cut.Number > 0 {
		tags["track"] = fmt.Sprintf("%d/%d", cut.Number, sheet.AudioTrackCount())
	}
	if sheet.DiscNumber != "" {
		disc := sheet.DiscNumber
		if sheet.TotalDiscs != "" {
			disc += "/" + sheet.TotalDiscs
		}
		tags["disc"] = disc
	}
	return tags
}

// performCueSplitWithCover режет исходник на треки и отправляет их.
// Lossless-источники (FLAC, APE, WavPack, WAV, TTA, TAK, AIFF, ALAC, DSD)
// уходят FLAC через юзербота или ALAC через Bot API; mp3/aac копируются без
// перекодирования, прочий lossy — в AAC.
func performCueSplitWithCover(c tele.Context, pcs *PendingCueSplit, coverData []byte) error {
	probe, err := probeAudio(pcs.AudioPath)
	if err != nil {
		return err
	}
	lossless := probe.Lossless()
	useUserbot := lossless && userbot.Ready()
	mode := lossyMode(probe)
	if lossless {
		mode = outALAC
		if useUserbot {
			mode = outFLAC
		}
	}
	outDir := filepath.Join(filepath.Dir(pcs.AudioPath), ".cue_"+pcs.fileHash())
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	log.Printf("[cue] %s: нарезка %d треков, кодек=%s (%s, %d бит, %d Гц) -> %s", pcs.AudioPath, len(pcs.Cuts), probe.Codec, probe.SampleFmt, probe.BitsRaw, probe.SampleRate, mode.ext())

	var lastErr error
	for i, cut := range pcs.Cuts {
		UpdateAudioProgress(pcs.RootTmp, fmt.Sprintf("🎼 Нарезка по cue: %s — трек %d из %d", html.EscapeString(filepath.Base(pcs.AudioPath)), i+1, len(pcs.Cuts)))

		end := cut.End
		stop := end
		if stop == 0 {
			stop = probe.Duration
		}
		if stop > 0 && stop <= cut.Start {
			log.Printf("[cue] %s: трек %d нулевой длительности (start=%v end=%v), пропуск", pcs.AudioPath, cut.Number, cut.Start, stop)
			continue
		}
		durSecs := int((stop - cut.Start).Seconds())

		cacheKey := fmt.Sprintf("%s#%d", audioCacheKey(pcs.Hash, pcs.FileID), cut.Number)
		if tgfid := db.GetTGFileID(cacheKey); tgfid != "" {
			if err := sendCachedAudio(c, tgfid, cut.Title, cut.Performer); err != nil {
				log.Printf("[cue] %s: трек %d не отправлен из кэша: %v", pcs.AudioPath, cut.Number, err)
				lastErr = err
			}
			continue
		}

		tags := cueTags(pcs.Sheet, cut)
		base := filepath.Join(outDir, fmt.Sprintf("%02d. %s", cut.Number, sanitizeFileName(cut.Title)))
		outPath := base + mode.ext()
		if err := transcodeAudio(pcs.AudioPath, outPath, cut.Start, end, probe, mode, tags); err != nil {
			lastErr = err
			continue
		}

		if mode == outFLAC {
			msgID, chatID, sendErr := userbot.SendToRelay(context.Background(), outPath, cut.Title, cut.Performer, durSecs, coverData)
			var sent *tele.Message
			if sendErr == nil {
				sent, sendErr = c.Bot().Copy(c.Recipient(), tele.StoredMessage{MessageID: strconv.Itoa(msgID), ChatID: chatID})
			}
			os.Remove(outPath)
			if sendErr == nil {
				if sent != nil && sent.Audio != nil && sent.Audio.FileID != "" {
					db.SaveTGFileID(cacheKey, sent.Audio.FileID)
				}
				continue
			}
			log.Printf("[cue] %s: трек %d не отправлен через userbot (%v), откатываюсь на Bot API (ALAC)", pcs.AudioPath, cut.Number, sendErr)
			outPath = base + outALAC.ext()
			if err := transcodeAudio(pcs.AudioPath, outPath, cut.Start, end, probe, outALAC, tags); err != nil {
				lastErr = err
				continue
			}
		}
		if err := sendAudio(c, outPath, cut.Performer, cut.Title, durSecs, coverData, cacheKey); err != nil {
			log.Printf("[cue] %s: трек %d не отправлен: %v", pcs.AudioPath, cut.Number, err)
			lastErr = err
		}
		os.Remove(outPath)
	}
	os.Remove(outDir)
	if lastErr == nil {
		os.Remove(pcs.AudioPath)
	}
	UpdateAudioProgress(pcs.RootTmp, "")
	return lastErr
}

func sendCachedAudio(c tele.Context, fileID, title, performer string) error {
	audio := &tele.Audio{File: tele.File{FileID: fileID}, Title: title, Performer: performer}
	_, err := c.Bot().Send(c.Recipient(), audio)
	return err
}
