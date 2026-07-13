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
	//   RegisterAudioTasks(tmpDir, 1, onDone, onBytes, onProgress) — в
	//     начале loading: "задача-страж", не даёт удалить папку, пока идёт
	//     цикл выгрузки (в т.ч. фото, документы и прочие неаудио-файлы).
	//     onDone вызывается РОВНО ОДИН раз, когда счётчик дойдёт до нуля —
	//     это и есть настоящий момент "всё точно готово" (а не момент,
	//     когда конвейер просто передал последний физический файл — для
	//     cue-нарезаемых альбомов это лишь начало интерактивной обработки).
	//     onBytes вызывается на КАЖДОМ закрытии задачи (см.
	//     AddAudioTask/CompleteAudioTask) с размером соответствующего
	//     физического файла — так wrk.uploadedBytes растёт по мере
	//     реальной отправки, а не сразу как только файл передан
	//     AudioProcessor'у. onProgress вызывается МНОГО раз за жизнь любой
	//     из задач папки (сейчас — только из cue-нарезки, см.
	//     UpdateAudioProgress) — статус-сообщение иначе висело бы
	//     неизменным всё время нарезки (может занимать минуты).
	//   AddAudioTask(tmpDir, bytes) — перед каждой передачей файла в
	//     AudioProcessor; bytes — размер этого файла (file.Length).
	//   CompleteAudioTask(tmpDir) — в конце loading закрывает стража.
	// Папка удаляется, когда счётчик доходит до нуля: цикл завершён И все
	// интерактивные аудиозадачи (выбор обложки) обработаны.
	RegisterAudioTasks func(tmpDir string, count int, onDone func(), onBytes func(int64), onProgress func(string))
	AddAudioTask       func(tmpDir string, bytes int64)
	CompleteAudioTask  func(tmpDir string)

	// MirrorToDrive — опциональный хук резервного копирования скачанных
	// файлов во внешнее хранилище (см. tgbot/gdrive), вызывается сразу
	// после успешного скачивания каждого файла, ДО выгрузки в Telegram.
	// Не должен блокировать или прерывать выгрузку в Telegram надолго и не
	// должен паниковать/возвращать ошибку наружу — всё это обязана решать
	// сама реализация хука (best-effort, ошибки только логируются). nil,
	// если бэкап не сконфигурирован — тогда просто не вызывается.
	MirrorToDrive func(torrentTitle, localPath string)

	// DriveMirrorActive сообщает, включён ли и авторизован ли прямо сейчас
	// бэкап на Google Drive — используется только для финальной сводки в
	// чате (loading()), не влияет на сам MirrorToDrive. nil, если бэкап не
	// сконфигурирован вовсе (тогда считаем как false).
	DriveMirrorActive func() bool
)

type Worker struct {
	id          int
	c           tele.Context
	msg         *tele.Message
	torrentHash string
	isCancelled atomic.Bool
	fileIndices []int
	ti          *state.TorrentStatus
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
	// updateDownloadStatus) — скачивание и выгрузка теперь идут конкурентно
	// парами "файл за файлом" (см. runPipeline), поэтому оба поля
	// обновляются атомарно по мере завершения каждого файла.
	downloadedBytes atomic.Int64
	uploadedBytes   atomic.Int64
	// completedFiles — сколько файлов задачи уже полностью выгружено;
	// раньше жил локальной переменной внутри runAllFiles, недоступной
	// снаружи — вынесен на Worker, чтобы onBytes-колбэк (см.
	// RegisterAudioTasks в loading()) мог обновить статус-сообщение сразу,
	// когда очередной аудиофайл реально доставлен пользователем (а не
	// только тогда, когда конвейер сам завершает СЛЕДУЮЩИЙ файл — для
	// последнего файла задачи такого следующего вызова уже не будет).
	completedFiles atomic.Int32

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

	// pipelineFailed запоминает, завершился ли конвейер ошибкой — известно
	// только ПОСЛЕ runAllFiles, а onDone может понадобиться сильно позже
	// (когда закроется последняя интерактивная аудиозадача), поэтому решение
	// "показывать ли summary" принимается внутри onDone на момент его
	// вызова, а не на момент регистрации. atomic — onDone может выполниться
	// на другой горутине (той, что закрыла последнюю аудио-задачу).
	var pipelineFailed atomic.Bool
	totalFiles := len(wrk.fileIndices)
	onDone := func() {
		if pipelineFailed.Load() || wrk.isCancelled.Load() {
			return
		}
		log.Printf("[manager] loading: worker=%d hash=%s: успешно завершено", wrk.id, wrk.torrentHash)
		summary := fmt.Sprintf("✅ <b>%s</b>\nОтправлено файлов: %d", wrk.ti.Title, totalFiles)
		if DriveMirrorActive != nil && DriveMirrorActive() {
			summary += "\n💾 Резервная копия сохранена на Google Drive"
		}
		// Новым сообщением, а не правкой статуса — правка означает, что
		// сводка "готово" появляется НАД уже отправленными в чат файлами
		// (сообщение-статус было создано раньше всех них), из-за чего
		// выглядит так, будто её увидели раньше, чем реально дошли все
		// файлы. Отдельное сообщение снизу подтверждает завершение уже
		// ПОСЛЕ того, как всё оказалось в чате. Старое статус-сообщение
		// (с устаревшим "N из M"/промежуточным прогрессом) при этом
		// удаляется — иначе оно так и осталось бы висеть в чате навсегда
		// с неактуальными цифрами.
		wrk.c.Bot().Delete(wrk.msg)
		wrk.c.Bot().Send(wrk.c.Recipient(), summary, tele.ModeHTML)
	}

	// onProgress — промежуточный статус долгих интерактивных задач (сейчас
	// только cue-нарезка, см. UpdateAudioProgress/performCueSplitWithCover)
	// — троттлится тем же механизмом (throttleStatusUpdate), что и обычный
	// прогресс скачивания/выгрузки, чтобы не заваливать Bot API правками
	// на каждый трек большого альбома.
	onProgress := func(text string) {
		if pipelineFailed.Load() || wrk.isCancelled.Load() {
			return
		}
		if !wrk.throttleStatusUpdate(3 * time.Second) {
			return
		}
		wrk.c.Bot().Edit(wrk.msg, text, tele.ModeHTML)
	}

	// Задача-страж: пока цикл выгрузки не завершён, временную папку
	// удалять нельзя — из неё выгружаются в том числе неаудио-файлы
	// (фото, документы). Закрывается через defer при любом исходе
	// (успех, ошибка, отмена). Папка будет реально удалена, а onDone
	// вызван, когда счётчик задач в tgbot/audio.go дойдёт до нуля — для
	// cue-нарезаемых альбомов это может случиться сильно позже, чем
	// вернётся runAllFiles ниже (нарезка и отправка треков идёт
	// асинхронно, после того как пользователь ответит на меню).
	// onBytes зачисляет байты уже реально доставленного (не просто
	// переданного AudioProcessor'у) файла и ТУТ ЖЕ обновляет статус —
	// иначе счётчик рос бы честно, но пользователь видел бы это только
	// когда конвейер сам обработает СЛЕДУЮЩИЙ файл (а для последнего файла
	// задачи такого следующего вызова уже не будет вовсе, и прогресс так
	// и останется на "0.00%", несмотря на реально отправленные треки).
	onBytes := func(n int64) {
		wrk.uploadedBytes.Add(n)
		if pipelineFailed.Load() || wrk.isCancelled.Load() {
			return
		}
		wrk.reportUploadProgress(totalFiles, int(wrk.completedFiles.Load()))
	}

	hooksReady := RegisterAudioTasks != nil && AddAudioTask != nil && CompleteAudioTask != nil
	if hooksReady {
		RegisterAudioTasks(tmpDir, 1, onDone, onBytes, onProgress)
		defer CompleteAudioTask(tmpDir)
	} else {
		// Хуки не привязаны (аудио-обработка отключена) — папку чистим
		// сами по завершении, но onDone всё равно должен сработать здесь же
		// (иначе для отключённой аудио-обработки completion-сообщение
		// пропадёт насовсем).
		defer func() {
			os.RemoveAll(tmpDir)
			onDone()
		}()
	}

	if AudioProcessor != nil {
		prefetchCueSheets(wrk)
		prefetchFolderImages(wrk)
	}

	iserr := runAllFiles(wrk, totalFiles)
	if iserr {
		pipelineFailed.Store(true)
	}
}

// pipelineConcurrency — сколько файлов ОДНОВРЕМЕННО выгружаются в Telegram
// (см. runPipeline). Раньше файлы сначала скачивались ВСЕ, потом
// выгружались все — раздача вроде игры (сотни ГБ суммарно) должна была
// целиком поместиться на диск сервера разом; теперь скачивание идёт своим
// чередом (см. ниже, почему — строго последовательно) и передаёт готовые
// файлы выгрузке через канал, так что пиковое место на диске — это
// примерно pipelineConcurrency файлов (буфер канала), а не вся раздача.
// Значение то же, что было у прежней uploadConcurrency — подобрано
// консервативно, чтобы не упереться в flood-control локального Bot API
// сервера.
const pipelineConcurrency = 3

// runAllFiles прогоняет через конвейер (см. runPipeline) все файлы задачи
// ОДНИМ проходом, в том же порядке, в каком они перечислены в самом
// торренте (wrk.fileIndices) — раньше картинки (обложки и т.п.) шли
// отдельной, полностью дождавшейся себя фазой ПЕРЕД всем остальным, чтобы
// гарантировать, что обложка окажется на диске раньше, чем откроется меню
// её выбора для аудио. Это гарантированно доставляло обложку в чат раньше
// треков, но и переупорядочивало доставку (мелкие файлы вроде логов,
// идущие в торренте позже, оказывались в чате вперемешку с аудио, а не по
// порядку). Компромисс принят осознанно: если обложка папки в самом
// торренте перечислена ПОСЛЕ аудиотреков этой же папки (нетипично), меню
// выбора обложки для этой папки может показаться до того как обложка
// скачается, и картинки не будет среди вариантов — редкий краевой случай,
// не поломка. Удаление файлов конвейером эту гарантию не затрагивает:
// картинки и так не удаляются поштучно (см. ownedByAudioProcessor) — они
// живут до конца всей задачи независимо от порядка фаз.
// Возвращает true, если хотя бы один файл завершился ошибкой (в этом
// случае пользователю уже отправлено сообщение об ошибке).
func runAllFiles(wrk *Worker, totalFiles int) bool {
	wrk.reportUploadProgress(totalFiles, 0)

	var downloaded atomic.Int32
	if firstErr, firstErrFile := runPipeline(wrk, totalFiles, wrk.fileIndices, &downloaded, &wrk.completedFiles); firstErr != nil {
		errstr := fmt.Sprintf("Ошибка обработки файла: %v\n\n%v", firstErrFile, firstErr.Error())
		wrk.c.Bot().Edit(wrk.msg, errstr, tele.ModeHTML)
		return true
	}
	return false
}

// downloadedFile — файл, готовый к выгрузке (уже на диске), передаётся от
// продюсера к потребителям через канал в runPipeline.
type downloadedFile struct {
	fileIndex int
	tmpPath   string
}

// runPipeline скачивает файлы задачи и выгружает их в Telegram по схеме
// "один продюсер, несколько потребителей":
//
//   - Скачивание — СТРОГО ПОСЛЕДОВАТЕЛЬНО, одной горутиной. Раздача качается
//     из общего роя одного и того же торрента: несколько одновременных
//     скачиваний РАЗНЫХ файлов делят между собой одну и ту же пропускную
//     способность, а не складывают её — параллелить тут нечего, скорость
//     ограничена роем, а не нашим кодом. (Best-effort зеркалирование во
//     внешнее хранилище — см. MirrorToDrive — тоже происходит здесь, сразу
//     после скачивания каждого файла.)
//   - Выгрузка в Telegram — конкурентно, до pipelineConcurrency файлов
//     одновременно: тут параллелизм оправдан, каждый апload — независимый
//     сетевой I/O к Bot API (см. комментарий на uploadConcurrency в старой
//     версии этого файла).
//
// Буфер канала (pipelineConcurrency) даёт скачиванию уйти на несколько
// файлов вперёд выгрузки, не тратя место на диске сверх этого — именно это
// и ограничивает пиковое использование диска раздачей вроде игры (сотни ГБ
// суммарно), а не вся раздача целиком, как было раньше.
//
// downloaded/completed — общие на всю задачу счётчики (переиспользуются
// между фазами runAllFiles, чтобы прогресс-бар считал по всей задаче, а не
// обнулялся между фазой картинок и остального). Останавливается на первой
// ошибке: новые файлы не стартуют (ни на скачивание, ни на выгрузку), но
// уже скачанные и ждущие в канале — доигрываются.
func runPipeline(wrk *Worker, totalFiles int, fileIndices []int, downloaded, completed *atomic.Int32) (firstErr error, firstErrFile string) {
	if len(fileIndices) == 0 {
		return nil, ""
	}

	ch := make(chan downloadedFile, pipelineConcurrency)
	var mu sync.Mutex

	go func() {
		defer close(ch)
		for _, fileIndex := range fileIndices {
			if wrk.isCancelled.Load() {
				return
			}
			mu.Lock()
			stop := firstErr != nil
			mu.Unlock()
			if stop {
				return
			}

			file := wrk.ti.FileStats[fileIndex]
			dlStart := time.Now()
			tmpPath, err := downloadFileToDisk(wrk, file, int(downloaded.Load())+1, totalFiles)
			if err != nil {
				log.Printf("[manager] loading: worker=%d: скачивание файла %q FAILED после %v: %v", wrk.id, file.Path, time.Since(dlStart), err)
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					firstErrFile = file.Path
				}
				mu.Unlock()
				return
			}
			downloaded.Add(1)
			log.Printf("[manager] loading: worker=%d: файл %q скачан за %v -> %s", wrk.id, file.Path, time.Since(dlStart), tmpPath)

			if MirrorToDrive != nil && tmpPath != "" {
				MirrorToDrive(wrk.ti.Title, tmpPath)
			}

			ch <- downloadedFile{fileIndex: fileIndex, tmpPath: tmpPath}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < pipelineConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for df := range ch {
				if wrk.isCancelled.Load() {
					continue
				}
				mu.Lock()
				stop := firstErr != nil
				mu.Unlock()
				if stop {
					continue
				}

				file := wrk.ti.FileStats[df.fileIndex]
				upStart := time.Now()
				err := uploadFileFromDisk(wrk, file, df.tmpPath)
				if err != nil {
					log.Printf("[manager] loading: worker=%d: выгрузка файла %q FAILED после %v: %v", wrk.id, file.Path, time.Since(upStart), err)
					mu.Lock()
					if firstErr == nil {
						firstErr = err
						firstErrFile = file.Path
					}
					mu.Unlock()
					continue
				}
				log.Printf("[manager] loading: worker=%d: файл %q выгружен за %v", wrk.id, file.Path, time.Since(upStart))
				// Учитываем байты файла в совокупном прогрессе ПОСЛЕ реальной
				// отправки — КРОМЕ файлов, отданных в AudioProcessor
				// (интерактивный выбор обложки, cue-нарезка): для них
				// возврат uploadFileFromDisk означает лишь "меню показано",
				// а не "файл действительно доставлен" — для таких файлов
				// wrk.uploadedBytes зачтёт байты позже, через onBytes в
				// RegisterAudioTasks (см. tgbot/audio.go), когда трек(и)
				// реально уйдут пользователю. Условие то же, что решает
				// ownedByAudioProcessor — только БЕЗ картинок: их обработка
				// (в т.ч. решение пропустить отправку) уже полностью
				// завершена к этому моменту, откладывать нечего.
				if !(isAudioExt(file.Path) && ownedByAudioProcessor(file, df.tmpPath)) {
					wrk.uploadedBytes.Add(file.Length)
				}
				done := completed.Add(1)
				wrk.reportUploadProgress(totalFiles, int(done))

				// Локальную копию можно стереть сразу — освобождает место на
				// диске для следующих файлов конвейера — кроме файлов, за
				// которыми ещё присматривает AudioProcessor (интерактивный
				// выбор обложки, cue-нарезка): их жизненным циклом управляет
				// счётчик RegisterAudioTasks/AddAudioTask/CompleteAudioTask
				// (tgbot/audio.go), а не этот вызов. tmpPath == "" — файл уже
				// был в кэше Telegram, на диске его и не было.
				if df.tmpPath != "" && !ownedByAudioProcessor(file, df.tmpPath) {
					os.Remove(df.tmpPath)
				}
			}
		}()
	}
	wg.Wait()
	return firstErr, firstErrFile
}

// ownedByAudioProcessor сообщает, будет ли файл (после успешной выгрузки)
// ещё некоторое время нужен AudioProcessor'у асинхронно — тогда runPipeline
// не должен удалять его сам. Условие ДОЛЖНО совпадать с тем, что реально
// решает uploadFileFromDisk перед вызовом AudioProcessor — включая гейт по
// размеру: файл >= safePartSize уходит в AudioProcessor, только если это
// ещё и cue-кандидат (isCueSplitCandidate + hasSiblingCueFile); иначе он
// целиком уходит в ProcessLargeFile (7z), который НЕ трогает исходный файл
// — тогда удалять его обязан именно конвейер, как обычный файл. Раньше
// этот гейт здесь не проверялся, из-за чего большие не-cue аудиофайлы
// (аудиокниги, m4a/mp3 за порогом) молча не удалялись до конца всей задачи
// — то есть именно тот риск по месту на диске, ради которого затевался
// весь конвейер (task A), но теперь уже на аудио-файлах.
//
// Картинки тоже защищены от немедленного удаления, если аудио-обработка
// вообще включена: runAllFiles теперь идёт ОДНИМ проходом в порядке
// торрента (без отдельной "картинки первыми" фазы, см. её комментарий) —
// картинка аудио-папки может докачаться раньше или позже самих треков в
// зависимости от их порядка в торренте, и именно эти файлы на диске служат
// источником обложки для меню выбора, когда в самом аудиофайле её нет (см.
// findImagesInDir в processAudioFileNormally и offerCueCoverSelection в
// cue.go). Раз конвейер не удаляет картинку сама сразу после её выгрузки
// (а держит до конца задачи, см. RegisterAudioTasks/CompleteAudioTask), она
// остаётся на диске и доступна меню независимо от момента её собственной
// выгрузки в чат. Сами картинки маленькие — держать их до конца задачи не
// создаёт того риска по месту на диске, ради которого затевался весь
// конвейер — тот был именно про большие файлы.
func ownedByAudioProcessor(file *state.TorrentFileStat, diskPath string) bool {
	if isImageExt(file.Path) {
		return AudioProcessor != nil
	}
	if strings.EqualFold(filepath.Ext(file.Path), ".cue") {
		// .cue тоже нужен живым на диске дольше, чем длится его СОБСТВЕННАЯ
		// выгрузка — prefetchCueSheets кладёт его туда заранее специально
		// для offerCueSplit (tgbot/cue.go), который может дойти до него
		// лишь сильно позже, когда докачается связанный аудиофайл (тот
		// обычно в разы больше и качается намного дольше крошечного .cue).
		// Раньше .cue, если пользователь выбрал его наравне с треками,
		// уходил как обычный документ и удалялся конвейером почти сразу
		// (за секунды) — к моменту, когда аудиофайл доходил до
		// ProcessAudioFile, cue-sheet уже пропадал с диска, и предложение
		// нарезать не показывалось вовсе, хотя cue был валиден.
		return AudioProcessor != nil
	}
	if !isAudioExt(file.Path) || AudioProcessor == nil || !isProcessableAudio(file.Path) {
		return false
	}
	if file.Length >= safePartSize {
		return isCueSplitCandidate(file.Path) && hasSiblingCueFile(diskPath)
	}
	return true
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
	// Реальный прогресс скачивания (а не захардкоженные "100%, завершено")
	// — конвейер выгружает уже скачанные файлы, пока следующие ещё
	// качаются, так что скачивание всей задачи в этот момент может быть
	// ещё не завершено. wrk.downloadedBytes — тот же счётчик, что
	// использует updateDownloadStatus.
	downloadedNow := wrk.downloadedBytes.Load()
	downloadPercent := 0.0
	if totalBytes > 0 {
		downloadPercent = float64(downloadedNow) / float64(totalBytes) * 100.0
	}
	if downloadPercent > 100.0 {
		downloadPercent = 100.0
	}

	msg += "📥 <b>Скачивание на сервер:</b>\n"
	msg += fmt.Sprintf("Прогресс: [%s] %.2f%%\n", GetProgressBar(downloadPercent), downloadPercent)
	if downloadPercent >= 100.0 {
		msg += "✅ <i>Успешно завершено</i>\n\n"
	} else {
		msg += fmt.Sprintf("Данные: %s / %s\n\n", humanize.Bytes(uint64(downloadedNow)), humanize.Bytes(uint64(totalBytes)))
	}
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

	// Картинка могла быть заранее скачана prefetchFolderImages (см.
	// prefetch_images.go) — чтобы она уже лежала на диске к моменту, когда
	// какой-то аудиофайл её папки откроет меню выбора обложки, даже если
	// сама картинка перечислена в торренте ПОСЛЕ аудио. Если файл уже на
	// месте и нужного размера — не качаем повторно, просто досчитываем
	// прогресс и отдаём путь как обычно.
	if isImageExt(file.Path) {
		relPath := strings.TrimPrefix(file.Path, "/")
		fullPath := filepath.Join(wrk.tmpDir, relPath)
		if info, statErr := os.Stat(fullPath); statErr == nil && info.Size() == file.Length {
			log.Printf("[manager] downloadFileToDisk: %q уже скачан заранее, повторное скачивание пропущено", file.Path)
			wrk.downloadedBytes.Add(file.Length)
			return fullPath, nil
		}
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
			updateDownloadStatus(wrk, torrFile, fi, fc, false)
			time.Sleep(1 * time.Second)
		}
		wa.Done()
	}()

	// io.Copy гоняет данные через дефолтный буфер в 32 КБ — для файла в
	// несколько ГБ по локальному loopback (TorrServer на этой же машине)
	// это десятки тысяч пар read/write-syscall'ов на файл. Буфер побольше
	// снижает их число и ускоряет копирование заметно дороже, чем можно
	// было бы ожидать от "всего лишь" локальной передачи.
	copyBuf := make([]byte, 1<<20) // 1 МБ
	_, copyErr := io.CopyBuffer(tmpFile, torrFile, copyBuf)
	complete.Store(true)
	wa.Wait()

	if wrk.isCancelled.Load() {
		return "", errors.New("скачивание отменено пользователем")
	}
	if copyErr != nil {
		return "", copyErr
	}

	// Финальное принудительное обновление (мимо троттлинга): последний
	// периодический тик мог захватить снимок за секунду ДО реального
	// завершения копирования (например, "99.45%"), и без этого вызова
	// пользователь так и видел бы этот чуть-чуть недокачанный процент
	// висящим в статусе всё время, пока файл потом обрабатывается
	// (конвертация, cue-нарезка и т.п.) — до следующего события конвейера.
	updateDownloadStatus(wrk, torrFile, fi, fc, true)

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
		// file.Length передаётся вместе с задачей — так wrk.uploadedBytes
		// зачтёт его только когда файл ДЕЙСТВИТЕЛЬНО будет доставлен (см.
		// ownedByAudioProcessor ниже и audioTaskEntry в tgbot/audio.go), а
		// не сразу после того, как эта функция вернёт управление (что для
		// cue-нарезки означает только "меню показано").
		if AddAudioTask != nil {
			AddAudioTask(wrk.tmpDir, file.Length)
		}
		oversized := file.Length >= safePartSize
		var fallback func() error
		if oversized {
			fallback = buildLargeFileFallback()
		}
		return AudioProcessor(wrk.c, diskPath, wrk.torrentHash, wrk.tmpDir, file.Id, oversized, fallback)
	}

	if isImageExt(file.Path) && AudioProcessor != nil {
		// Картинки здесь больше не отправляются вовсе — по решению
		// пользователя единственный показ теперь происходит как превью-
		// документ в меню выбора обложки (см. offerCoverSelection в
		// tgbot/audio.go), если картинка окажется кандидатом обложки
		// какой-то аудио-папки. Файл при этом остаётся на диске
		// (ownedByAudioProcessor не даёт конвейеру его удалить) — доступен
		// findImagesInDir независимо от того, что здесь его не отправили.
		return nil
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
		// Остальные файлы (документы и т.п.) отправляются как есть, без
		// пережатия.
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

func updateDownloadStatus(wrk *Worker, file *TorrFile, fi, fc int, force bool) {
	if wrk.msg == nil {
		return
	}
	if !force && !wrk.throttleStatusUpdate(5*time.Second) {
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

	// Реальный прогресс выгрузки (а не захардкоженный "0%, ожидание") —
	// конвейер (см. runPipeline) выгружает уже скачанные файлы конкурентно
	// со скачиванием следующих, так что к этому моменту какие-то файлы
	// вполне могут быть уже выгружены. wrk.uploadedBytes — тот же счётчик,
	// что использует reportUploadProgress.
	uploadedNow := wrk.uploadedBytes.Load()
	uploadPercent := 0.0
	if totalBytes > 0 {
		uploadPercent = float64(uploadedNow) / float64(totalBytes) * 100.0
	}
	if uploadPercent > 100.0 {
		uploadPercent = 100.0
	}

	msg += "📤 <b>Выгрузка в Telegram:</b>\n"
	msg += fmt.Sprintf("Прогресс: [%s] %.2f%%\n", GetProgressBar(uploadPercent), uploadPercent)
	if uploadedNow > 0 {
		msg += fmt.Sprintf("Данные: %s / %s\n\n", humanize.Bytes(uint64(uploadedNow)), totalStr)
	} else {
		msg += "⏳ <i>Ожидание скачивания файлов...</i>\n\n"
	}

	msg += "⚙️ <code>" + file.hash + "</code>"

	torrKbd := &tele.ReplyMarkup{}
	torrKbd.Inline([]tele.Row{torrKbd.Row(torrKbd.Data("Отмена", "cancel", strconv.Itoa(wrk.id)))}...)
	wrk.c.Bot().Edit(wrk.msg, msg, torrKbd, tele.ModeHTML)
}
