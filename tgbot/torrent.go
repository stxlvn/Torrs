package tgbot

import (
	"errors"
	"fmt"
	"hash/crc32"
	"log"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	tele "gopkg.in/telebot.v4"
	"torrsru/tgbot/torr"
	"torrsru/tgbot/torr/state"
)

// Состояние сессии пользователя для конкретного торрента
type UserState struct {
	CurrentDir string
	Page       int
	Selections map[int]bool // id файла -> выбрано
}

var (
	userStates sync.Map // Ключ: "{chatID}_{hash}" -> *UserState
	pathMap    sync.Map // Ключ: uint32 (хэш chatID+hash+пути) -> *pathEntry
)

const PageSize = 10

// pathMapTTL — время жизни записи в pathMap. Раньше записи не удалялись
// вообще, что приводило к неограниченному росту памяти на долгоживущем
// процессе (запись создаётся на каждую отрисованную папку каждого
// пользователя). Хэш также раньше считался только от FullPath, из-за чего
// два разных пользователя (или два разных торрента) могли получить
// одинаковый 32-битный хэш и попасть в чужую папку — теперь ключ включает
// chatID и hash торрента.
const pathMapTTL = 6 * time.Hour

func init() {
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		for range ticker.C {
			now := time.Now()
			pathMap.Range(func(k, v interface{}) bool {
				if now.Sub(v.(*pathEntry).ts) > pathMapTTL {
					pathMap.Delete(k)
				}
				return true
			})
		}
	}()
}

type pathEntry struct {
	path string
	ts   time.Time
}

func pathMapKey(chatID int64, hash, fullPath string) uint32 {
	return crc32.ChecksumIEEE([]byte(fmt.Sprintf("%d|%s|%s", chatID, hash, fullPath)))
}

// naturalLess сравнивает строки "по-человечески": числовые подстроки
// сравниваются как числа, а не посимвольно — иначе plain "<" ставит "10"
// перед "2" (лексикографически "1" < "2"). Разбивает обе строки на
// чередующиеся куски цифр/нецифр и сравнивает кусок за куском.
func naturalLess(a, b string) bool {
	ai, bi := 0, 0
	for ai < len(a) && bi < len(b) {
		ac, bc := a[ai], b[bi]
		if isDigit(ac) && isDigit(bc) {
			aStart, bStart := ai, bi
			for ai < len(a) && isDigit(a[ai]) {
				ai++
			}
			for bi < len(b) && isDigit(b[bi]) {
				bi++
			}
			aNum := strings.TrimLeft(a[aStart:ai], "0")
			bNum := strings.TrimLeft(b[bStart:bi], "0")
			if len(aNum) != len(bNum) {
				return len(aNum) < len(bNum)
			}
			if aNum != bNum {
				return aNum < bNum
			}
			continue
		}
		if ac != bc {
			return ac < bc
		}
		ai++
		bi++
	}
	return len(a)-ai < len(b)-bi
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

// Получает или создает состояние пользователя
func getUserState(chatID int64, hash string) *UserState {
	key := fmt.Sprintf("%d_%s", chatID, hash)
	if v, ok := userStates.Load(key); ok {
		return v.(*UserState)
	}
	us := &UserState{
		CurrentDir: "",
		Page:       0,
		Selections: make(map[int]bool),
	}
	userStates.Store(key, us)
	return us
}

func infoTorrent(c tele.Context, magnet string) error {
	log.Printf("[torrent] infoTorrent: user=%d запрос=%s", c.Sender().ID, magnet)
	msg, err := c.Bot().Send(c.Recipient(), "🔍 Ищу информацию о раздаче... <code>"+magnet+"</code>", tele.ModeHTML)
	if err != nil {
		log.Printf("[torrent] infoTorrent: не удалось отправить сообщение: %v", err)
		return err
	}
	ti, err := torr.GetTorrentInfo(magnet)
	if err != nil {
		log.Printf("[torrent] infoTorrent: GetTorrentInfo(%s) FAILED: %v", magnet, err)
		msgErr, _ := c.Bot().Edit(msg, "⚠️ Не удалось достучаться до торрента <code>"+magnet+"</code>", tele.ModeHTML)
		go func() { time.Sleep(5 * time.Second); c.Bot().Delete(msgErr) }()
		return err
	}
	log.Printf("[torrent] infoTorrent: %s -> hash=%s название=%q файлов=%d", magnet, ti.Hash, ti.Title, len(ti.FileStats))

	c.Bot().Delete(msg)

	// Если в торренте всего 1 файл, качаем сразу без меню
	if len(ti.FileStats) == 1 {
		torr.AddRange(c, ti.Hash, ti.FileStats[0].Id, ti.FileStats[0].Id)
		return nil
	}

	// Сбрасываем состояние при новом поиске
	key := fmt.Sprintf("%d_%s", c.Sender().ID, ti.Hash)
	userStates.Delete(key)

	us := getUserState(c.Sender().ID, ti.Hash)

	// АВТОРАСКРЫТИЕ КОРНЕВОЙ ПАПКИ (усилено очисткой слешей)
	if len(ti.FileStats) > 0 {
		commonRoot := ""
		isSingleRoot := true
		for _, f := range ti.FileStats {
			cleanPath := strings.TrimPrefix(f.Path, "/")
			idx := strings.Index(cleanPath, "/")
			if idx != -1 {
				rootFolder := cleanPath[:idx+1]
				if commonRoot == "" {
					commonRoot = rootFolder
				} else if commonRoot != rootFolder {
					isSingleRoot = false
					break
				}
			} else {
				isSingleRoot = false
				break
			}
		}
		if isSingleRoot && commonRoot != "" {
			us.CurrentDir = commonRoot
		}
	}

	return renderTorrentPage(c, ti)
}

// Элемент виртуальной файловой системы (Папка или Файл)
type VfsItem struct {
	IsFile   bool
	Name     string
	FullPath string
	FileID   int
	Size     int64
	Selected bool
}

func renderTorrentPage(c tele.Context, ti *state.TorrentStatus) error {
	us := getUserState(c.Sender().ID, ti.Hash)

	// Собираем виртуальную файловую систему для текущей директории
	dirSet := make(map[string]struct{})
	var items []VfsItem

	for _, f := range ti.FileStats {
		cleanPath := strings.TrimPrefix(f.Path, "/")
		if !strings.HasPrefix(cleanPath, us.CurrentDir) {
			continue
		}
		relPath := cleanPath[len(us.CurrentDir):]

		if idx := strings.Index(relPath, "/"); idx != -1 {
			dirName := relPath[:idx]
			if _, exists := dirSet[dirName]; !exists {
				dirSet[dirName] = struct{}{}
				items = append(items, VfsItem{
					IsFile:   false,
					Name:     dirName,
					FullPath: us.CurrentDir + dirName + "/",
				})
			}
		} else {
			items = append(items, VfsItem{
				IsFile:   true,
				Name:     relPath,
				FullPath: f.Path,
				FileID:   f.Id,
				Size:     f.Length,
				Selected: us.Selections[f.Id],
			})
		}
	}

	// Сортируем: папки сверху, потом файлы; внутри группы — по "естественному"
	// порядку (числа сравниваются как числа: 2 < 10, а не "10" < "2").
	sort.Slice(items, func(i, j int) bool {
		if items[i].IsFile != items[j].IsFile {
			return !items[i].IsFile
		}
		return naturalLess(items[i].Name, items[j].Name)
	})

	totalItems := len(items)
	totalPages := (totalItems + PageSize - 1) / PageSize
	if totalPages == 0 {
		totalPages = 1
	}
	if us.Page >= totalPages {
		us.Page = totalPages - 1
	}

	displayDir := us.CurrentDir
	if displayDir == "" {
		displayDir = "/"
	}
	txt := fmt.Sprintf("<b>%s</b>\n<code>%s</code>\n\n📁 <b>Путь:</b> <code>%s</code>\n(Стр. %d из %d):",
		ti.Title, ti.Hash, displayDir, us.Page+1, totalPages)

	filesKbd := &tele.ReplyMarkup{}
	var rows []tele.Row

	start := us.Page * PageSize
	end := start + PageSize
	if end > totalItems {
		end = totalItems
	}

	for _, item := range items[start:end] {
		if item.IsFile {
			status := "⬜️ "
			if item.Selected {
				status = "🟩 "
			}
			btnText := status + item.Name + " (" + humanize.Bytes(uint64(item.Size)) + ")"
			btn := filesKbd.Data(btnText, "\ftog", ti.Hash, strconv.Itoa(item.FileID))
			rows = append(rows, filesKbd.Row(btn))
		} else {
			// Хэш для пути папки (чтобы влезть в callback_data), скопирован
			// на пользователя и торрент — см. pathMapKey.
			h := pathMapKey(c.Sender().ID, ti.Hash, item.FullPath)
			pathMap.Store(h, &pathEntry{path: item.FullPath, ts: time.Now()})
			dirHash := strconv.FormatUint(uint64(h), 10)

			// Кнопка входа в папку
			btnFolder := filesKbd.Data("📁 "+item.Name, "\fdir", ti.Hash, dirHash)

			// Чекбокс для папки: переключает выбор всех файлов внутри
			isFullySelected := isDirFullySelected(ti, us, item.FullPath)
			checkIcon := "⬜️"
			if isFullySelected {
				checkIcon = "🟩"
			}
			btnToggle := filesKbd.Data(checkIcon, "\ftogd", ti.Hash, dirHash)

			rows = append(rows, filesKbd.Row(btnFolder, btnToggle))
		}
	}

	// Навигация (вверх, страницы)
	var navRow []tele.Btn
	if us.CurrentDir != "" {
		navRow = append(navRow, filesKbd.Data("⬆️ Наверх", "\fup", ti.Hash))
	}
	if us.Page > 0 {
		navRow = append(navRow, filesKbd.Data("⬅️", "\fpage", ti.Hash, strconv.Itoa(us.Page-1)))
	}
	navRow = append(navRow, filesKbd.Data(fmt.Sprintf("📄 %d/%d", us.Page+1, totalPages), "\fnoop"))
	if us.Page < totalPages-1 {
		navRow = append(navRow, filesKbd.Data("➡️", "\fpage", ti.Hash, strconv.Itoa(us.Page+1)))
	}
	rows = append(rows, navRow)

	// Панель управления
	btnDown := filesKbd.Data("📥 Скачать выбранное", "\fdown", ti.Hash)
	rows = append(rows, filesKbd.Row(btnDown))

	btnAllDir := filesKbd.Data("🚀 Скачать эту папку", "\fdall", ti.Hash)
	btnCancel := filesKbd.Data("❌ Отмена", "\fcanc", ti.Hash)
	rows = append(rows, filesKbd.Row(btnAllDir, btnCancel))

	filesKbd.Inline(rows...)

	if c.Callback() != nil {
		_, err := c.Bot().Edit(c.Message(), txt, filesKbd, tele.ModeHTML)
		return err
	}
	return c.Send(txt, filesKbd, tele.ModeHTML)
}

// Вспомогательная функция: является ли расширение файла изображением для обложки
func isImageFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".bmp", ".gif":
		return true
	}
	return false
}

func getTorrent(c tele.Context) error {
	args := c.Args()
	for i := range args {
		args[i] = strings.TrimSpace(args[i])
	}
	if len(args) == 0 {
		return errors.New("Ошибка: нет аргументов")
	}
	cmd := args[0]
	if !strings.HasPrefix(cmd, "\f") {
		cmd = "\f" + cmd
	}
	log.Printf("[torrent] getTorrent: user=%d cmd=%s args=%v", c.Sender().ID, cmd, args[1:])

	// Пустышка для индикатора страницы
	if cmd == "\fnoop" {
		return c.Respond()
	}

	// Отмена и закрытие меню
	if cmd == "\fcanc" {
		if len(args) >= 2 {
			key := fmt.Sprintf("%d_%s", c.Sender().ID, args[1])
			userStates.Delete(key)
		}
		c.Bot().Delete(c.Message())
		return c.Respond(&tele.CallbackResponse{Text: "Меню закрыто"})
	}

	// Отмена загрузки (очередь)
	if cmd == "\fcancel" {
		if len(args) != 2 {
			return errors.New("Ошибка данных")
		}
		num, err := strconv.Atoi(args[1])
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "Неверный ID"})
		}
		torr.Cancel(num)
		c.Bot().Delete(c.Callback().Message)
		return c.Respond(&tele.CallbackResponse{Text: "Отменено"})
	}

	// Переход внутрь папки
	if cmd == "\fdir" {
		if len(args) != 3 {
			return errors.New("Ошибка данных")
		}
		hash := args[1]
		dirHash, _ := strconv.ParseUint(args[2], 10, 32)

		if entry, ok := pathMap.Load(uint32(dirHash)); ok {
			us := getUserState(c.Sender().ID, hash)
			us.CurrentDir = entry.(*pathEntry).path
			us.Page = 0
		}

		ti, err := torr.GetTorrentInfo(hash)
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "Ошибка торрента", ShowAlert: true})
		}
		renderTorrentPage(c, ti)
		return c.Respond()
	}

	// Возврат на уровень вверх
	if cmd == "\fup" {
		if len(args) != 2 {
			return errors.New("Ошибка данных")
		}
		hash := args[1]
		us := getUserState(c.Sender().ID, hash)

		trimmed := strings.TrimSuffix(us.CurrentDir, "/")
		lastSlash := strings.LastIndex(trimmed, "/")
		if lastSlash == -1 {
			us.CurrentDir = "" // Корень
		} else {
			us.CurrentDir = trimmed[:lastSlash+1]
		}
		us.Page = 0

		ti, err := torr.GetTorrentInfo(hash)
		if err != nil {
			return c.Respond()
		}
		renderTorrentPage(c, ti)
		return c.Respond()
	}

	// Скачать всю текущую папку (автоматически включает все картинки)
	if cmd == "\fdall" {
		if len(args) != 2 {
			return errors.New("Ошибка данных")
		}
		hash := args[1]
		us := getUserState(c.Sender().ID, hash)

		ti, err := torr.GetTorrentInfo(hash)
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "Ошибка торрента", ShowAlert: true})
		}

		var files []*state.TorrentFileStat
		for _, f := range ti.FileStats {
			cleanPath := strings.TrimPrefix(f.Path, "/")
			if strings.HasPrefix(cleanPath, us.CurrentDir) {
				files = append(files, f)
			}
		}
		sort.Slice(files, func(i, j int) bool {
			return naturalLess(files[i].Path, files[j].Path)
		})

		count := 0
		for _, f := range files {
			torr.AddRange(c, hash, f.Id, f.Id)
			count++
		}

		if count == 0 {
			return c.Respond(&tele.CallbackResponse{Text: "Папка пуста!", ShowAlert: true})
		}

		key := fmt.Sprintf("%d_%s", c.Sender().ID, hash)
		userStates.Delete(key)
		c.Bot().Edit(c.Message(), fmt.Sprintf("✅ Добавлено файлов в очередь из папки: %d", count))
		return c.Respond(&tele.CallbackResponse{Text: "Загрузка папки началась"})
	}

	// Переключение страницы
	if cmd == "\fpage" {
		if len(args) != 3 {
			return errors.New("Ошибка данных")
		}
		hash := args[1]
		page, _ := strconv.Atoi(args[2])

		us := getUserState(c.Sender().ID, hash)
		us.Page = page

		ti, err := torr.GetTorrentInfo(hash)
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "Ошибка загрузки данных", ShowAlert: true})
		}
		renderTorrentPage(c, ti)
		return c.Respond()
	}

	// Переключение выбора файла
	if cmd == "\ftog" {
		if len(args) != 3 {
			return errors.New("Ошибка данных")
		}
		hash := args[1]
		id, _ := strconv.Atoi(args[2])

		us := getUserState(c.Sender().ID, hash)
		us.Selections[id] = !us.Selections[id]

		ti, err := torr.GetTorrentInfo(hash)
		if err != nil {
			return c.Respond()
		}
		renderTorrentPage(c, ti)
		return c.Respond()
	}

	// Переключение выбора всей папки (ЧЕКБОКС)
	if cmd == "\ftogd" {
		if len(args) != 3 {
			return errors.New("Ошибка данных")
		}
		hash := args[1]
		dirHash, _ := strconv.ParseUint(args[2], 10, 32)

		entry, ok := pathMap.Load(uint32(dirHash))
		if !ok {
			return c.Respond(&tele.CallbackResponse{
				Text:      "Данные устарели. Откройте меню заново.",
				ShowAlert: true,
			})
		}
		path := entry.(*pathEntry).path

		// Мгновенно отвечаем, чтобы кнопка не зависала
		c.Respond(&tele.CallbackResponse{Text: "Переключаем..."})

		us := getUserState(c.Sender().ID, hash)
		ti, err := torr.GetTorrentInfo(hash)
		if err != nil {
			c.Bot().Edit(c.Message(), "⚠️ Ошибка получения данных торрента")
			return nil
		}

		newState := !isDirFullySelected(ti, us, path)
		for _, f := range ti.FileStats {
			cleanPath := strings.TrimPrefix(f.Path, "/")
			if strings.HasPrefix(cleanPath, path) {
				us.Selections[f.Id] = newState
			}
		}
		renderTorrentPage(c, ti)
		return nil
	}

	// Скачать выбранные файлы (автоматически добавляем картинки из тех же папок, что и аудио)
	if cmd == "\fdown" {
		if len(args) != 2 {
			return errors.New("Ошибка данных")
		}
		hash := args[1]
		us := getUserState(c.Sender().ID, hash)

		ti, err := torr.GetTorrentInfo(hash)
		if err != nil {
			return c.Respond(&tele.CallbackResponse{Text: "Ошибка получения данных", ShowAlert: true})
		}

		// Собираем множество папок, в которых есть выбранные файлы
		selectedDirs := make(map[string]bool) // ключ – виртуальный путь к папке
		idToPath := make(map[int]string)
		for _, f := range ti.FileStats {
			cleanPath := strings.TrimPrefix(f.Path, "/")
			idToPath[f.Id] = cleanPath
			if us.Selections[f.Id] {
				dir := filepath.Dir(cleanPath)
				if dir != "." {
					selectedDirs[dir+"/"] = true
				}
			}
		}

		// Добавляем все файлы-картинки, лежащие в этих папках
		for _, f := range ti.FileStats {
			if !us.Selections[f.Id] && isImageFile(f.Path) {
				cleanPath := strings.TrimPrefix(f.Path, "/")
				dir := filepath.Dir(cleanPath)
				if dir != "." {
					dir += "/"
				}
				if selectedDirs[dir] {
					us.Selections[f.Id] = true // помечаем как выбранное, чтобы добавить в загрузку
				}
			}
		}

		var selectedIDs []int
		for id, sel := range us.Selections {
			if sel {
				selectedIDs = append(selectedIDs, id)
			}
		}
		if len(selectedIDs) == 0 {
			return c.Respond(&tele.CallbackResponse{Text: "Вы ничего не выбрали!", ShowAlert: true})
		}

		sort.Slice(selectedIDs, func(i, j int) bool {
			return naturalLess(idToPath[selectedIDs[i]], idToPath[selectedIDs[j]])
		})

		count := 0
		for _, id := range selectedIDs {
			torr.AddRange(c, hash, id, id)
			count++
		}

		key := fmt.Sprintf("%d_%s", c.Sender().ID, hash)
		userStates.Delete(key)
		c.Bot().Edit(c.Message(), "✅ Добавлено файлов в очередь: "+strconv.Itoa(count))
		return c.Respond(&tele.CallbackResponse{Text: "Загрузка началась"})
	}

	// === ОБРАБОТЧИКИ ОБЛОЖЕК ===
	// Выбор обложки из списка
	if cmd == "\fcover" {
		if len(args) != 4 {
			return errors.New("Ошибка данных")
		}
		hash := args[1]
		idx, _ := strconv.Atoi(args[2])
		dirHash := args[3]
		return handleCoverSelection(c, hash, idx, dirHash)
	}

	// Пропустить выбор обложки
	if cmd == "\fskip" {
		if len(args) != 3 {
			return errors.New("Ошибка данных")
		}
		hash := args[1]
		dirHash := args[2]
		return handleCoverSkip(c, hash, dirHash)
	}

	// Загрузка своей обложки
	if cmd == "\fcovup" {
		if len(args) != 3 {
			return errors.New("Ошибка данных")
		}
		hash := args[1]
		dirHash := args[2]
		uploadExpect.Store(c.Sender().ID, uploadInfo{Hash: hash, DirHash: dirHash})
		return c.Respond(&tele.CallbackResponse{Text: "Отправьте изображение для обложки"})
	}

	// === ОБРАБОТЧИКИ CUE-SHEET ===
	if cmd == "\fcuesplit" {
		if len(args) != 3 {
			return errors.New("Ошибка данных")
		}
		return handleCueSplitConfirm(c, args[1], args[2])
	}

	if cmd == "\fcueskip" {
		if len(args) != 3 {
			return errors.New("Ошибка данных")
		}
		return handleCueSplitDecline(c, args[1], args[2])
	}

	log.Printf("[torrent] getTorrent: user=%d неизвестная команда cmd=%s", c.Sender().ID, cmd)
	return errors.New("Неизвестная команда")
}

// Проверяет, выбраны ли все файлы внутри указанной папки
func isDirFullySelected(ti *state.TorrentStatus, us *UserState, path string) bool {
	for _, f := range ti.FileStats {
		cleanPath := strings.TrimPrefix(f.Path, "/")
		if strings.HasPrefix(cleanPath, path) && !us.Selections[f.Id] {
			return false
		}
	}
	return true
}

func isHash(txt string) bool {
	if len(txt) == 40 {
		for _, c := range strings.ToLower(txt) {
			switch c {
			case 'a', 'b', 'c', 'd', 'e', 'f', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			default:
				return false
			}
		}
		return true
	}
	return false
}
