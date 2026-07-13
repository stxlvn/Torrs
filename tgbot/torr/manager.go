package torr

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dhowden/tag"
	"github.com/dustin/go-humanize"
	tele "gopkg.in/telebot.v4"
	"torrsru/db"
	"torrsru/tgbot/torr/state"
)

// safePartSize — порог, после которого файл считается "большим" и
// отправляется через LargeFileProcessor (нарезка на тома 7-Zip).
const safePartSize = 1_900_000_000

var (
	// fileID — id файла внутри торрента (state.TorrentFileStat.Id), нужен
	// AudioProcessor'у для ключа кэша Telegram file_id (db.SaveTGFileID),
	// чтобы при повторном запросе того же трека/торрента не качать и не
	// обрабатывать заново, а переслать уже отправленный файл.
	// oversized сообщает, что файл превышает safePartSize и в обход был
	// пропущен к AudioProcessor как кандидат на нарезку по cue; fallback
	// (не nil ровно тогда, когда oversized) — откат на LargeFileProcessor
	// (7z-архивация), если cue в итоге не подтвердится/не найдётся.
	AudioProcessor     func(c tele.Context, filePath string, hash string, tmpDir string, fileID int, oversized bool, fallback func() error) error
	LargeFileProcessor func(c tele.Context, filePath string, fileSize int64, fileName string, hash string, statusMsg *tele.Message, isCancelled func() bool, kbd *tele.ReplyMarkup) error

	// Хуки учёта задач по временной папке торрента (реализация — в
	// tgbot/audio.go, привязка — в tgbot/bot.go). Схема:
	//   RegisterAudioTasks(tmpDir, 1) — в начале loading: "задача-страж",
	//     не даёт удалить папку, пока идёт цикл выгрузки (в т.ч. фото,
	//     документы и прочие неаудио-файлы).
	//   AddAudioTask(tmpDir) — перед каждой передачей файла в AudioProcessor.
	//   CompleteAudioTask(tmpDir) — в конце loading закрывает стража.
	// Папка удаляется, когда счётчик доходит до нуля: цикл завершён И все
	// интерактивные аудиозадачи (выбор обложки) обработаны.
	RegisterAudioTasks func(tmpDir string, count int)
	AddAudioTask       func(tmpDir string)
	CompleteAudioTask  func(tmpDir string)
)

type Worker struct {
	id          int
	c           tele.Context
	msg         *tele.Message
	torrentHash string
	isCancelled atomic.Bool
	fileIndices []int
	ti          *state.TorrentStatus
	cachedFiles map[int]string
	tmpDir      string

	// totalBytes — суммарный размер всех файлов задачи, посчитан один раз
	// при формировании fileIndices (AddRange) и неизменен с момента, когда
	// задача покидает очередь и переходит в loading(). Используется для
	// отображения СОВОКУПНОГО прогресса по всей задаче, а не только по
	// одному текущему файлу.
	totalBytes int64
	// downloadedBytes/uploadedBytes — накопленный прогресс по уже
	// полностью обработанным файлам. Текущий (ещё не завершённый) файл
	// добавляет к этой сумме свой live-прогресс отдельно (см.
	// updateDownloadStatus) — скачивание идёт строго последовательно, а
	// выгрузка теперь конкурентна (см. uploadAllFiles), поэтому
	// uploadedBytes обновляется атомарно по мере завершения каждого файла.
	downloadedBytes atomic.Int64
	uploadedBytes   atomic.Int64

	// statusMu защищает lastStatusUpdate от гонок — выгрузка файлов теперь
	// идёт параллельно (uploadConcurrency горутин), и без мьютекса
	// троттлинг статус-сообщения был бы гонкой данных.
	statusMu         sync.Mutex
	lastStatusUpdate time.Time
}

// throttleStatusUpdate возвращает true не чаще, чем раз в minInterval —
// используется, чтобы не заваливать Telegram Edit-запросами при частых
// обновлениях прогресса (в т.ч. из нескольких горутин параллельной выгрузки).
func (wrk *Worker) throttleStatusUpdate(minInterval time.Duration) bool {
	wrk.statusMu.Lock()
	defer wrk.statusMu.Unlock()
	if time.Since(wrk.lastStatusUpdate) < minInterval {
		return false
	}
	wrk.lastStatusUpdate = time.Now()
	return true
}

type Manager struct {
	queue     []*Worker
	working   map[int]*Worker
	ids       int
	wrkSync   sync.Mutex
	queueLock sync.Mutex
}

func (m *Manager) Start() {
	m.working = make(map[int]*Worker)
	go m.work()
}

func (m *Manager) AddRange(c tele.Context, hash string, from, to int) {
	log.Printf("[manager] AddRange: user=%d hash=%s from=%d to=%d", c.Sender().ID, hash, from, to)
	m.queueLock.Lock()
	defer m.queueLock.Unlock()

	if len(m.queue) > 50 {
		log.Printf("[manager] AddRange: очередь переполнена (%d), отказ user=%d hash=%s", len(m.queue), c.Sender().ID, hash)
		c.Bot().Send(c.Recipient(), "Очередь переполнена, попробуйте попозже\n\nЭлементов в очереди:"+strconv.Itoa(len(m.queue)))
		return
	}

	// Идемпотентность: Telegram (в т.ч. локальный Bot API сервер) может
	// повторно доставить одно и то же обновление — например, если процесс
	// перезапустился до того, как успел уйти следующий getUpdates с
	// продвинутым offset. Раньше здесь проверялась только очередь
	// (m.queue), а уже АКТИВНО обрабатываемая задача (m.working) — нет,
	// из-за чего повторно доставленный колбэк "Скачать выбранное" запускал
	// скачивание и выгрузку (в т.ч. обложки/cue) заново с нуля, параллельно
	// с ещё идущей первой попыткой. В m.working задачу нельзя молча
	// "дозаполнить" файлами, как в очереди ниже: её fileIndices уже читает
	// в цикле loading() в отдельной горутине, и конкурентный append сюда
	// был бы гонкой данных — поэтому просто игнорируем дубликат.
	for _, w := range m.working {
		if w.torrentHash == hash && w.c.Sender().ID == c.Sender().ID {
			log.Printf("[manager] AddRange: hash=%s user=%d уже обрабатывается worker=%d — повторный запрос проигнорирован (вероятно, повторно доставленное обновление Telegram)", hash, c.Sender().ID, w.id)
			return
		}
	}

	var existingWrk *Worker
	for _, w := range m.queue {
		if w.torrentHash == hash && w.c.Sender().ID == c.Sender().ID {
			existingWrk = w
			break
		}
	}

	if existingWrk != nil {
		ti := existingWrk.ti
		if from == 1 && to == -1 {
			to = len(ti.FileStats)
		}
		if to > len(ti.FileStats) {
			to = len(ti.FileStats)
		}
		if from < 1 {
			from = 1
		}
		if to >= 0 && to < from {
			from, to = to, from
		}
		if to > len(ti.FileStats) {
			to = len(ti.FileStats)
		}

		for i := from - 1; i <= to-1; i++ {
			exists := false
			for _, existingI := range existingWrk.fileIndices {
				if existingI == i {
					exists = true
					break
				}
			}
			if !exists {
				existingWrk.fileIndices = append(existingWrk.fileIndices, i)
				existingWrk.totalBytes += ti.FileStats[i].Length
			}
		}

		log.Printf("[manager] AddRange: добавлено к существующей задаче worker=%d hash=%s, файлов теперь=%d", existingWrk.id, hash, len(existingWrk.fileIndices))
		m.sendQueueStatusLocked()
		return
	}

	m.ids++
	if m.ids > math.MaxInt {
		m.ids = 0
	}

	var msg *tele.Message
	var err error

	for i := 0; i < 20; i++ {
		msg, err = c.Bot().Send(c.Recipient(), "<b>Подключение к торренту</b>\n<code>"+hash+"</code>", tele.ModeHTML)
		if err == nil {
			break
		} else {
			log.Println("Error send msg, try again:", i+1, "/", 20)
		}
	}

	if err != nil {
		log.Println("Error send msg:", err)
		return
	}

	ti, tiErr := GetTorrentInfo(hash)
	if ti == nil {
		log.Printf("[manager] AddRange: GetTorrentInfo(%s) FAILED: %v", hash, tiErr)
		c.Bot().Edit(msg, "Ошибка при подключении к торренту <code>"+hash+"</code>", tele.ModeHTML)
		return
	}

	if from == 1 && to == -1 {
		to = len(ti.FileStats)
	}
	if to > len(ti.FileStats) {
		to = len(ti.FileStats)
	}
	if from < 1 {
		from = 1
	}
	if to >= 0 && to < from {
		from, to = to, from
	}
	if to > len(ti.FileStats) {
		to = len(ti.FileStats)
	}

	var fileIndices []int
	var totalBytes int64
	for i := from - 1; i <= to-1; i++ {
		fileIndices = append(fileIndices, i)
		totalBytes += ti.FileStats[i].Length
	}

	w := &Worker{
		id:          m.ids,
		c:           c,
		torrentHash: hash,
		msg:         msg,
		ti:          ti,
		fileIndices: fileIndices,
		cachedFiles: make(map[int]string),
		totalBytes:  totalBytes,
	}

	m.queue = append(m.queue, w)
	log.Printf("[manager] AddRange: новая задача worker=%d hash=%s файлов=%d название=%s", w.id, hash, len(fileIndices), ti.Title)
	m.sendQueueStatusLocked()
}

func (m *Manager) Cancel(id int) {
	log.Printf("[manager] Cancel: id=%d", id)
	m.queueLock.Lock()
	defer m.queueLock.Unlock()
	for i, w := range m.queue {
		if w.id == id {
			log.Printf("[manager] Cancel: worker=%d hash=%s отменён в очереди (позиция %d)", w.id, w.torrentHash, i+1)
			w.isCancelled.Store(true)
			w.c.Bot().Delete(w.msg)
			m.queue = append(m.queue[:i], m.queue[i+1:]...)
			return
		}
	}
	if wrk, ok := m.working[id]; ok {
		log.Printf("[manager] Cancel: worker=%d hash=%s отменён во время обработки", wrk.id, wrk.torrentHash)
		wrk.isCancelled.Store(true)
		return
	}
	log.Printf("[manager] Cancel: id=%d не найден ни в очереди, ни в работе", id)
}

func (m *Manager) work() {
	for {
		m.queueLock.Lock()
		if len(m.working) > 0 {
			m.queueLock.Unlock()
			m.sendQueueStatus()
			time.Sleep(time.Second)
			continue
		}
		if len(m.queue) == 0 {
			m.queueLock.Unlock()
			time.Sleep(time.Second)
			continue
		}
		wrk := m.queue[0]
		m.queue = m.queue[1:]
		m.working[wrk.id] = wrk
		m.queueLock.Unlock()

		log.Printf("[manager] work: старт обработки worker=%d hash=%s файлов=%d", wrk.id, wrk.torrentHash, len(wrk.fileIndices))
		m.sendQueueStatus()

		start := time.Now()
		loading(wrk)
		log.Printf("[manager] work: обработка worker=%d hash=%s завершена за %v (cancelled=%v)", wrk.id, wrk.torrentHash, time.Since(start), wrk.isCancelled.Load())

		m.queueLock.Lock()
		delete(m.working, wrk.id)
		m.queueLock.Unlock()
	}
}

func (m *Manager) sendQueueStatus() {
	m.queueLock.Lock()
	snapshot := m.queueSnapshotLocked()
	m.queueLock.Unlock()
	sendQueueStatusSnapshot(snapshot)
}

// sendQueueStatusLocked вызывается, когда queueLock уже удержан вызывающей
// стороной (AddRange). Снимок очереди берётся мгновенно под локом, а сами
// сетевые вызовы Edit к Telegram уходят в отдельную горутину — иначе N
// последовательных сетевых запросов держали бы queueLock и блокировали
// AddRange/Cancel на время своего выполнения.
func (m *Manager) sendQueueStatusLocked() {
	snapshot := m.queueSnapshotLocked()
	go sendQueueStatusSnapshot(snapshot)
}

type queueStatusItem struct {
	c           tele.Context
	msg         *tele.Message
	id          int
	position    int
	fileCount   int
	torrentHash string
}

func (m *Manager) queueSnapshotLocked() []queueStatusItem {
	snapshot := make([]queueStatusItem, 0, len(m.queue))
	for i, wrk := range m.queue {
		if wrk.msg == nil {
			continue
		}
		snapshot = append(snapshot, queueStatusItem{
			c:           wrk.c,
			msg:         wrk.msg,
			id:          wrk.id,
			position:    i + 1,
			fileCount:   len(wrk.fileIndices),
			torrentHash: wrk.torrentHash,
		})
	}
	return snapshot
}

func sendQueueStatusSnapshot(snapshot []queueStatusItem) {
	for _, item := range snapshot {
		torrKbd := &tele.ReplyMarkup{}
		torrKbd.Inline([]tele.Row{torrKbd.Row(torrKbd.Data("Отмена", "cancel", strconv.Itoa(item.id)))}...)

		msg := "⏳ <b>Очередь</b> (позиция " + strconv.Itoa(item.position) + ")\n"
		msg += "📦 <b>Файлов к обработке:</b> " + strconv.Itoa(item.fileCount) + "\n"
		msg += "⚙️ <code>" + item.torrentHash + "</code>"

		item.c.Bot().Edit(item.msg, msg, torrKbd, tele.ModeHTML)
	}
}

func GetProgressBar(percent float64) string {
	fill := int(percent / 10)
	if fill > 10 {
		fill = 10
	}
	if fill < 0 {
		fill = 0
	}
	s := ""
	for i := 0; i < fill; i++ {
		s += "█"
	}
	for i := fill; i < 10; i++ {
		s += "░"
	}
	return s
}

func loading(wrk *Worker) {
	log.Printf("[manager] loading: worker=%d hash=%s: старт, файлов=%d", wrk.id, wrk.torrentHash, len(wrk.fileIndices))
	tmpDir, err := os.MkdirTemp("", "torrdl_*")
	if err != nil {
		log.Printf("[manager] loading: worker=%d: не удалось создать временную папку: %v", wrk.id, err)
		wrk.c.Bot().Edit(wrk.msg, "Ошибка создания временной папки", tele.ModeHTML)
		return
	}
	wrk.tmpDir = tmpDir
	log.Printf("[manager] loading: worker=%d: временная папка %s", wrk.id, tmpDir)

	// Задача-страж: пока цикл выгрузки не завершён, временную папку
	// удалять нельзя — из неё выгружаются в том числе неаудио-файлы
	// (фото, документы). Закрывается через defer при любом исходе
	// (успех, ошибка, отмена). Папка будет реально удалена, когда
	// счётчик задач в tgbot/audio.go дойдёт до нуля.
	hooksReady := RegisterAudioTasks != nil && AddAudioTask != nil && CompleteAudioTask != nil
	if hooksReady {
		RegisterAudioTasks(tmpDir, 1)
		defer CompleteAudioTask(tmpDir)
	} else {
		// Хуки не привязаны (аудио-обработка отключена) — папку чистим
		// сами по завершении.
		defer os.RemoveAll(tmpDir)
	}

	if AudioProcessor != nil {
		prefetchCueSheets(wrk)
	}

	iserr := false
	totalFiles := len(wrk.fileIndices)

	for idx, fileIndex := range wrk.fileIndices {
		if wrk.isCancelled.Load() {
			log.Printf("[manager] loading: worker=%d: отменено на скачивании файла %d/%d", wrk.id, idx+1, totalFiles)
			return
		}
		file := wrk.ti.FileStats[fileIndex]
		dlStart := time.Now()
		tmpPath, err := downloadFileToDisk(wrk, file, idx+1, totalFiles)
		if err != nil {
			log.Printf("[manager] loading: worker=%d: скачивание файла %q FAILED после %v: %v", wrk.id, file.Path, time.Since(dlStart), err)
			errstr := fmt.Sprintf("Ошибка скачивания файла на сервер: %v\n\n%v", file.Path, err.Error())
			wrk.c.Bot().Edit(wrk.msg, errstr, tele.ModeHTML)
			iserr = true
			break
		}
		log.Printf("[manager] loading: worker=%d: файл %q скачан за %v -> %s", wrk.id, file.Path, time.Since(dlStart), tmpPath)
		wrk.cachedFiles[fileIndex] = tmpPath
	}

	if iserr || wrk.isCancelled.Load() {
		return
	}

	iserr = uploadAllFiles(wrk, totalFiles)

	if !iserr && !wrk.isCancelled.Load() {
		log.Printf("[manager] loading: worker=%d hash=%s: успешно завершено", wrk.id, wrk.torrentHash)
		wrk.c.Bot().Delete(wrk.msg)
	}
}

// uploadConcurrency — сколько файлов выгружаются в Telegram одновременно.
// Раньше файлы отправлялись строго по одному, хотя каждый апload — это
// сетевой I/O (десятки секунд на трек, см. логи sendWithRetry), а сама
// выгрузка (ffmpeg-конвертация/тегирование одного трека и сетевая отправка
// другого) не имеет общих зависимостей между разными файлами одной задачи —
// PendingCover/audioTaskCounts в audio.go уже защищены мьютексами/atomic
// именно для конкурентного доступа. Значение подобрано консервативно, чтобы
// не упереться в flood-control локального Bot API сервера.
const uploadConcurrency = 3

// uploadAllFiles выгружает файлы задачи с ограниченной конкурентностью.
// Изображения (обложки и т.п.) выгружаются ПЕРВОЙ, полностью завершённой
// фазой — и только потом всё остальное (аудио, документы). При
// uploadConcurrency > 1 порядок ЗАВЕРШЕНИЯ (а значит и порядок появления
// сообщений в чате) не совпадает с порядком запуска, поэтому простой
// сортировки wrk.fileIndices недостаточно — нужна отдельная, дождавшаяся
// себя фаза, чтобы обложка гарантированно пришла в чат раньше треков.
// Возвращает true, если хотя бы одна выгрузка завершилась ошибкой (в этом
// случае пользователю уже отправлено сообщение об ошибке).
func uploadAllFiles(wrk *Worker, totalFiles int) bool {
	var images, rest []int
	for _, fi := range wrk.fileIndices {
		if isImageExt(wrk.ti.FileStats[fi].Path) {
			images = append(images, fi)
		} else {
			rest = append(rest, fi)
		}
	}

	wrk.reportUploadProgress(totalFiles, 0)

	var completed atomic.Int32
	if firstErr, firstErrFile := uploadBatch(wrk, totalFiles, images, &completed); firstErr != nil {
		errstr := fmt.Sprintf("Ошибка выгрузки файла в телеграм: %v\n\n%v", firstErrFile, firstErr.Error())
		wrk.c.Bot().Edit(wrk.msg, errstr, tele.ModeHTML)
		return true
	}
	if wrk.isCancelled.Load() {
		return false
	}

	if firstErr, firstErrFile := uploadBatch(wrk, totalFiles, rest, &completed); firstErr != nil {
		errstr := fmt.Sprintf("Ошибка выгрузки файла в телеграм: %v\n\n%v", firstErrFile, firstErr.Error())
		wrk.c.Bot().Edit(wrk.msg, errstr, tele.ModeHTML)
		return true
	}
	return false
}

// uploadBatch выгружает один набор file-индексов с ограниченной
// конкурентностью (uploadConcurrency), обновляя общий на всю задачу счётчик
// completed (переиспользуется между фазами uploadAllFiles, чтобы прогресс-бар
// считал по всей задаче, а не обнулялся между фазой картинок и остального).
// Останавливается на первой ошибке: новые загрузки из этого батча не
// стартуют, но уже запущенные — доигрываются.
func uploadBatch(wrk *Worker, totalFiles int, fileIndices []int, completed *atomic.Int32) (firstErr error, firstErrFile string) {
	sem := make(chan struct{}, uploadConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, fileIndex := range fileIndices {
		if wrk.isCancelled.Load() {
			break
		}
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop {
			break
		}

		sem <- struct{}{}
		wg.Add(1)
		go func(fileIndex int) {
			defer wg.Done()
			defer func() { <-sem }()

			if wrk.isCancelled.Load() {
				return
			}
			file := wrk.ti.FileStats[fileIndex]
			tmpPath := wrk.cachedFiles[fileIndex]
			upStart := time.Now()
			err := uploadFileFromDisk(wrk, file, tmpPath)
			if err != nil {
				log.Printf("[manager] loading: worker=%d: выгрузка файла %q FAILED после %v: %v", wrk.id, file.Path, time.Since(upStart), err)
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					firstErrFile = file.Path
				}
				mu.Unlock()
				return
			}
			log.Printf("[manager] loading: worker=%d: файл %q выгружен за %v", wrk.id, file.Path, time.Since(upStart))
			// Учитываем байты файла в совокупном прогрессе ПОСЛЕ реальной
			// отправки. Исключение — интерактивный выбор обложки в
			// audio.go: там ProcessAudioFile может вернуться раньше, чем
			// трек реально уйдёт пользователю (см. AddAudioTask), поэтому
			// для таких треков прогресс-бар обгонит фактическую отправку.
			wrk.uploadedBytes.Add(file.Length)
			done := completed.Add(1)
			wrk.reportUploadProgress(totalFiles, int(done))
		}(fileIndex)
	}
	wg.Wait()
	return firstErr, firstErrFile
}

// reportUploadProgress обновляет статус-сообщение совокупным прогрессом
// выгрузки по ВСЕЙ задаче (байты уже отправленных файлов / общий размер),
// а не только по одному "текущему" файлу — при конкурентной выгрузке
// (uploadConcurrency > 1) единственного "текущего" файла всё равно не
// существует. Троттлится, т.к. вызывается из нескольких горутин.
func (wrk *Worker) reportUploadProgress(totalFiles, completedFiles int) {
	if wrk.msg == nil {
		return
	}
	if !wrk.throttleStatusUpdate(3 * time.Second) {
		return
	}

	uploaded := wrk.uploadedBytes.Load()
	totalBytes := wrk.totalBytes
	percent := 0.0
	if totalBytes > 0 {
		percent = float64(uploaded) / float64(totalBytes) * 100.0
	}
	if percent > 100.0 {
		percent = 100.0
	}

	ti, _ := GetTorrentInfo(wrk.torrentHash)
	title := wrk.torrentHash
	if ti != nil && ti.Title != "" {
		title = ti.Title
	}

	msg := "🚀 <b>Обработка торрента...</b>\n\n"
	msg += "💿 <b>Название:</b> " + title + "\n"
	if totalFiles > 1 {
		msg += "📦 <b>Файлы:</b> " + strconv.Itoa(completedFiles) + " из " + strconv.Itoa(totalFiles) + "\n\n"
	} else {
		msg += "\n"
	}
	msg += "📥 <b>Скачивание на сервер:</b>\n"
	msg += fmt.Sprintf("Прогресс: [%s] 100.00%%\n", GetProgressBar(100.0))
	msg += "✅ <i>Успешно завершено</i>\n\n"
	msg += "📤 <b>Выгрузка в Telegram:</b>\n"
	msg += fmt.Sprintf("Прогресс: [%s] %.2f%%\n", GetProgressBar(percent), percent)
	msg += fmt.Sprintf("Данные: %s / %s\n\n", humanize.Bytes(uint64(uploaded)), humanize.Bytes(uint64(totalBytes)))
	msg += "⚙️ <code>" + wrk.torrentHash + "</code>"

	torrKbd := &tele.ReplyMarkup{}
	torrKbd.Inline([]tele.Row{torrKbd.Row(torrKbd.Data("Отмена", "cancel", strconv.Itoa(wrk.id)))}...)
	wrk.c.Bot().Edit(wrk.msg, msg, torrKbd, tele.ModeHTML)
}

func isImageExt(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".bmp", ".gif",
		".tif", ".tiff", ".jfif", ".heic", ".heif", ".avif":
		return true
	}
	return false
}

// maxDownloadRetries — сколько раз повторить скачивание файла с TorrServer
// при сетевом сбое (оборванное соединение к 127.0.0.1:8090, unexpected EOF
// и т.п.), прежде чем считать задачу окончательно проваленной. Раньше
// единственный такой сбой обрывал всю задачу целиком и требовал от
// пользователя запускать закачку заново — при том, что для отправки в
// Telegram (см. sendWithRetry/sendVolumeWithRetry) ретраи уже были.
const maxDownloadRetries = 5

func downloadFileToDisk(wrk *Worker, file *state.TorrentFileStat, fi, fc int) (string, error) {
	tgfid := ""
	if !isImageExt(file.Path) {
		tgfid = db.GetTGFileID(wrk.torrentHash + "|" + strconv.Itoa(file.Id))
	}
	if tgfid != "" {
		log.Printf("[manager] downloadFileToDisk: %q уже есть в кэше Telegram (fileID), скачивание пропущено", file.Path)
		wrk.downloadedBytes.Add(file.Length)
		return "", nil
	}

	var lastErr error
	for attempt := 1; attempt <= maxDownloadRetries; attempt++ {
		if wrk.isCancelled.Load() {
			return "", errors.New("скачивание отменено пользователем")
		}

		fullPath, err := downloadFileToDiskOnce(wrk, file, fi, fc)
		if err == nil {
			wrk.downloadedBytes.Add(file.Length)
			return fullPath, nil
		}
		if wrk.isCancelled.Load() {
			// Отмена пользователем — не сетевой сбой, ретраить не нужно.
			return "", errors.New("скачивание отменено пользователем")
		}

		lastErr = err
		log.Printf("[manager] downloadFileToDisk: %q попытка %d/%d FAILED: %v", file.Path, attempt, maxDownloadRetries, err)
		if attempt < maxDownloadRetries {
			time.Sleep(3 * time.Second)
		}
	}
	return "", lastErr
}

// downloadFileToDiskOnce — одна попытка скачивания. Каждая попытка заново
// открывает поток и создаёт (с усечением) файл на диске — частично
// скачанные данные от предыдущей неудачной попытки не переиспользуются.
func downloadFileToDiskOnce(wrk *Worker, file *state.TorrentFileStat, fi, fc int) (string, error) {
	torrFile, err := NewTorrFile(wrk, file)
	if err != nil {
		log.Printf("[manager] downloadFileToDisk: не удалось открыть поток %q: %v", file.Path, err)
		return "", err
	}
	defer torrFile.Close()

	relPath := file.Path
	if strings.HasPrefix(relPath, "/") {
		relPath = relPath[1:]
	}
	fullPath := filepath.Join(wrk.tmpDir, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return "", err
	}

	tmpFile, err := os.Create(fullPath)
	if err != nil {
		return "", err
	}
	defer tmpFile.Close()

	var wa sync.WaitGroup
	wa.Add(1)
	var complete atomic.Bool

	go func() {
		for !complete.Load() {
			if wrk.isCancelled.Load() {
				complete.Store(true)
				break
			}
			updateDownloadStatus(wrk, torrFile, fi, fc)
			time.Sleep(1 * time.Second)
		}
		wa.Done()
	}()

	_, copyErr := io.Copy(tmpFile, torrFile)
	complete.Store(true)
	wa.Wait()

	if wrk.isCancelled.Load() {
		return "", errors.New("скачивание отменено пользователем")
	}
	if copyErr != nil {
		return "", copyErr
	}

	return fullPath, nil
}

// isProcessableAudio — форматы, которые обрабатывает интерактивный
// AudioProcessor (теги + обложки). ВАЖНО: список должен совпадать с
// проверкой расширений в tgbot/audio.go ProcessAudioFile. Форматы вроде
// .wav/.aac считаются аудио (isAudioExt), но идут обычным путём отправки —
// раньше они попадали в AudioProcessor, который их молча игнорировал,
// и такие файлы вообще не отправлялись пользователю.
func isProcessableAudio(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp3", ".flac", ".m4a", ".ogg":
		return true
	}
	return false
}

func uploadFileFromDisk(wrk *Worker, file *state.TorrentFileStat, diskPath string) error {
	if wrk.isCancelled.Load() {
		return errors.New("выгрузка отменена пользователем")
	}
	if diskPath == "" {
		// downloadFileToDisk отдаёт "" только тогда, когда файл уже был
		// отправлен раньше и его FileID закэширован (см. db.GetTGFileID
		// там же) — раньше этот случай молча игнорировался и закэшированный
		// файл вообще не доходил до пользователя повторно.
		if !isImageExt(file.Path) {
			if tgfid := db.GetTGFileID(wrk.torrentHash + "|" + strconv.Itoa(file.Id)); tgfid != "" {
				return sendCachedFile(wrk, file, tgfid)
			}
		}
		log.Printf("[manager] uploadFileFromDisk: %q: пустой diskPath и файл не в кэше — пропуск без отправки", file.Path)
		return nil
	}

	caption := filepath.Base(file.Path)

	isAudio := isAudioExt(file.Path)

	// Кандидат на нарезку по cue (целый альбом одним FLAC + .cue рядом)
	// пропускает порог safePartSize: после нарезки отдельные треки в разы
	// меньше исходного альбома и 7z-архивация им не нужна. Раньше такие
	// хайрез-альбомы (24/192 и т.п., которые запросто превышают 1.9 ГБ
	// одним файлом) всегда уходили в архив, даже не долетев до
	// AudioProcessor — пользователь вообще не видел предложения нарезать.
	isCueCandidate := isAudio && AudioProcessor != nil && isProcessableAudio(file.Path) &&
		isCueSplitCandidate(file.Path) && hasSiblingCueFile(diskPath)

	buildLargeFileFallback := func() func() error {
		if LargeFileProcessor == nil {
			return nil
		}
		// Кнопку строим здесь и передаём явно — раньше LargeFileProcessor
		// брал её из wrk.msg.ReplyMarkup, а это поле никогда не обновляется
		// после Edit() (возвращаемое значение везде отбрасывается), из-за
		// чего оно оставалось nil и первый же Edit внутри архивации стирал
		// кнопку "Отмена" с реального сообщения в Telegram.
		torrKbd := &tele.ReplyMarkup{}
		torrKbd.Inline([]tele.Row{torrKbd.Row(torrKbd.Data("Отмена", "cancel", strconv.Itoa(wrk.id)))}...)
		return func() error {
			return LargeFileProcessor(wrk.c, diskPath, file.Length, file.Path, wrk.torrentHash, wrk.msg, wrk.isCancelled.Load, torrKbd)
		}
	}

	// Статус-сообщение здесь больше не редактируется точечно под этот один
	// файл — при конкурентной выгрузке (см. uploadAllFiles) "текущий файл"
	// перестал быть осмысленным понятием (одновременно грузится несколько).
	// Совокупный прогресс по всей задаче показывает reportUploadProgress,
	// вызываемый из uploadAllFiles до старта и после завершения каждого
	// файла.
	if file.Length >= safePartSize && !isCueCandidate {
		fallback := buildLargeFileFallback()
		if fallback != nil {
			return fallback()
		}
		return errors.New("файл превышает 1.9 ГБ, разбиение не настроено")
	}

	if isAudio && AudioProcessor != nil && isProcessableAudio(file.Path) {
		// Регистрируем интерактивную аудиозадачу ДО вызова обработчика:
		// её закроет tgbot/audio.go, когда трек будет реально отправлен
		// (в т.ч. после того как пользователь выберет обложку, подтвердит
		// нарезку по cue, либо она в итоге откатится на 7z-архивацию).
		if AddAudioTask != nil {
			AddAudioTask(wrk.tmpDir)
		}
		oversized := file.Length >= safePartSize
		var fallback func() error
		if oversized {
			fallback = buildLargeFileFallback()
		}
		return AudioProcessor(wrk.c, diskPath, wrk.torrentHash, wrk.tmpDir, file.Id, oversized, fallback)
	}

	fileReader, err := os.Open(diskPath)
	if err != nil {
		return err
	}
	defer fileReader.Close()

	var sendable interface{}
	if isAudio {
		audio := &tele.Audio{
			FileName: file.Path,
			Caption:  caption,
		}
		if m, err := tag.ReadFrom(fileReader); err == nil {
			if m.Title() != "" {
				audio.Title = m.Title()
			}
			if m.Artist() != "" {
				audio.Performer = m.Artist()
			}
			if pic := m.Picture(); pic != nil {
				audio.Thumbnail = &tele.Photo{File: tele.FromReader(bytes.NewReader(pic.Data))}
			}
		}
		fileReader.Seek(0, io.SeekStart)
		audio.File = tele.FromReader(fileReader)
		sendable = audio
	} else {
		// Фото и остальные файлы отправляются как документы (без
		// пережатия). Раньше картинки здесь пропускались (return nil)
		// и вообще не доходили до пользователя.
		doc := &tele.Document{
			FileName: file.Path,
			Caption:  caption,
			File:     tele.FromReader(fileReader),
		}
		sendable = doc
	}

	return sendWithRetry(wrk, file, sendable)
}

// sendWithRetry отправляет sendable (*tele.Audio или *tele.Document) с
// повторными попытками при сетевых ошибках и сохраняет полученный от
// Telegram FileID в кэше, чтобы повторные запросы того же файла отдавались
// без повторного скачивания/выгрузки.
// retryAfterRe вылавливает "retry after N" из текста ошибки. Нужен как
// fallback: локальный self-hosted Bot API сервер иногда отдаёт flood-control
// с error_code=400 ("Bad Request") вместо стандартного 429, и telebot в
// этом случае НЕ оборачивает ошибку в FloodError (см. extractOk в
// bot_raw.go — там на code=429 завязана сборка FloodError), хотя текст
// описания всё равно содержит "retry after N".
var retryAfterRe = regexp.MustCompile(`retry after (\d+)`)

// FloodRetryDelay вычисляет паузу перед повторной попыткой отправки.
// Telegram при flood control явно указывает, сколько секунд ждать — раньше
// это игнорировалось, и ретрай с фиксированной паузой (2-5с) снова и снова
// упирался в тот же самый лимит, пока не истощал все попытки. Если ошибка
// не про flood-control, отдаём fallback.
func FloodRetryDelay(err error, fallback time.Duration) time.Duration {
	var floodErr tele.FloodError
	if errors.As(err, &floodErr) && floodErr.RetryAfter > 0 {
		return time.Duration(floodErr.RetryAfter)*time.Second + time.Second
	}
	if err != nil {
		if m := retryAfterRe.FindStringSubmatch(err.Error()); m != nil {
			if secs, convErr := strconv.Atoi(m[1]); convErr == nil && secs > 0 {
				return time.Duration(secs)*time.Second + time.Second
			}
		}
	}
	return fallback
}

func sendWithRetry(wrk *Worker, file *state.TorrentFileStat, sendable interface{}) error {
	var sendErr error
	for i := 0; i < 20; i++ {
		if wrk.isCancelled.Load() {
			return errors.New("выгрузка отменена пользователем")
		}
		sendErr = wrk.c.Send(sendable)
		if sendErr == nil || errors.Is(sendErr, ERR_STOPPED) {
			break
		} else {
			delay := FloodRetryDelay(sendErr, 2*time.Second)
			log.Printf("[manager] sendWithRetry: %q попытка %d/20 FAILED: %v (ждём %v)", file.Path, i+1, sendErr, delay)
			time.Sleep(delay)
		}
	}

	if errors.Is(sendErr, ERR_STOPPED) {
		sendErr = nil
	} else if sendErr != nil {
		log.Printf("[manager] sendWithRetry: %q окончательно FAILED: %v", file.Path, sendErr)
	} else {
		if a, ok := sendable.(*tele.Audio); ok && a.FileID != "" {
			db.SaveTGFileID(wrk.torrentHash+"|"+strconv.Itoa(file.Id), a.FileID)
		} else if d, ok := sendable.(*tele.Document); ok && d.FileID != "" {
			db.SaveTGFileID(wrk.torrentHash+"|"+strconv.Itoa(file.Id), d.FileID)
		}
	}

	return sendErr
}

// sendCachedFile отправляет файл, для которого уже известен Telegram FileID
// (сохранённый sendWithRetry при прошлой отправке того же файла из другого
// торрента/запроса) — без повторного скачивания и выгрузки байтов.
func sendCachedFile(wrk *Worker, file *state.TorrentFileStat, tgfid string) error {
	log.Printf("[manager] sendCachedFile: %q отправляется из кэша Telegram (fileID)", file.Path)
	caption := filepath.Base(file.Path)
	var sendable interface{}
	if isAudioExt(file.Path) {
		sendable = &tele.Audio{File: tele.File{FileID: tgfid}, FileName: file.Path, Caption: caption}
	} else {
		sendable = &tele.Document{File: tele.File{FileID: tgfid}, FileName: file.Path, Caption: caption}
	}
	return sendWithRetry(wrk, file, sendable)
}

func isAudioExt(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp3", ".flac", ".m4a", ".wav", ".ogg", ".aac":
		return true
	}
	return false
}

func updateDownloadStatus(wrk *Worker, file *TorrFile, fi, fc int) {
	if wrk.msg == nil {
		return
	}
	if !wrk.throttleStatusUpdate(5 * time.Second) {
		return
	}

	ti, err := GetTorrentInfo(wrk.torrentHash)
	if err != nil {
		return
	}
	if wrk.isCancelled.Load() {
		return
	}

	if ti.DownloadSpeed == 0 {
		ti.DownloadSpeed = 1.0
	}

	wait := time.Duration(float64(file.Loaded())/ti.DownloadSpeed) * time.Second
	speed := humanize.Bytes(uint64(ti.DownloadSpeed)) + "/sec"
	peers := fmt.Sprintf("%v (%v/%v)", ti.ConnectedSeeders, ti.ActivePeers, ti.TotalPeers)

	// Совокупный прогресс по ВСЕЙ задаче: байты уже полностью скачанных
	// файлов + live-прогресс текущего файла. Раньше здесь показывался
	// только процент/размер текущего файла — при закачке альбома из 10
	// треков пользователь не видел общий прогресс по всем 10 сразу.
	totalBytes := wrk.totalBytes
	downloadedNow := wrk.downloadedBytes.Load() + file.offset
	globalPercent := 0.0
	if totalBytes > 0 {
		globalPercent = float64(downloadedNow) / float64(totalBytes) * 100.0
	}
	if globalPercent > 100.0 {
		globalPercent = 100.0
	}

	downloadedStr := humanize.Bytes(uint64(downloadedNow))
	totalStr := humanize.Bytes(uint64(totalBytes))

	msg := "🚀 <b>Обработка торрента...</b>\n\n"
	msg += "💿 <b>Название:</b> " + ti.Title + "\n"
	if fc > 1 {
		msg += "📦 <b>Файлы:</b> " + strconv.Itoa(fi) + " из " + strconv.Itoa(fc) + "\n\n"
	} else {
		msg += "\n"
	}

	msg += "📥 <b>Скачивание на сервер:</b>\n"
	if file.offset < file.size {
		msg += fmt.Sprintf("Прогресс: [%s] %.2f%%\n", GetProgressBar(globalPercent), globalPercent)
		msg += fmt.Sprintf("Данные: %s / %s\n", downloadedStr, totalStr)
		msg += fmt.Sprintf("Скорость: %s | Пиры: %s\n", speed, peers)
		msg += fmt.Sprintf("Осталось: %s\n\n", wait.String())
	} else {
		msg += fmt.Sprintf("Прогресс: [%s] %.2f%%\n", GetProgressBar(globalPercent), globalPercent)
		msg += "⏳ <i>Финализация файла...</i>\n\n"
	}

	msg += "📤 <b>Выгрузка в Telegram:</b>\n"
	msg += fmt.Sprintf("Прогресс: [%s] 0.00%%\n", GetProgressBar(0.0))
	msg += "⏳ <i>Ожидание скачивания файлов...</i>\n\n"

	msg += "⚙️ <code>" + file.hash + "</code>"

	torrKbd := &tele.ReplyMarkup{}
	torrKbd.Inline([]tele.Row{torrKbd.Row(torrKbd.Data("Отмена", "cancel", strconv.Itoa(wrk.id)))}...)
	wrk.c.Bot().Edit(wrk.msg, msg, torrKbd, tele.ModeHTML)
}
