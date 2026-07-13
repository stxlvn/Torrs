package tgbot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dhowden/tag"
	tele "gopkg.in/telebot.v4"
	"torrsru/db"
	"torrsru/tgbot/torr"
	"torrsru/tgbot/userbot"
)

// PendingCover описывает состояние выбора обложки для ОДНОЙ ПАПКИ торрента.
// Внутри папки может быть несколько треков — все они используют одно и то
// же решение по обложке. Так как обработка треков асинхронна (мы не блокируем
// очередь закачки на время, пока пользователь тыкает кнопки), треки, которые
// приходят ДО того как решение принято, складываются в AudioPaths и
// обрабатываются все разом, когда пользователь наконец отвечает.
// queuedTrack — трек папки, ожидающий решения по обложке, с уже прочитанными
// тегами. Метаданные читаются один раз (в ProcessAudioFile, через ffprobe/тег
// парсер) и переиспользуются здесь — без этого readAudioInfo вызывалась бы
// повторно для того же файла в момент, когда решение по обложке принято.
type queuedTrack struct {
	Path     string
	Artist   string
	Title    string
	Duration int
	CacheKey string // db.SaveTGFileID ключ, см. audioCacheKey
}

type PendingCover struct {
	AudioPaths []queuedTrack // треки папки, ожидающие решения по обложке
	AudioDir   string
	Hash       string
	Images     []string
	Selected   string // путь к выбранной картинке-обложке (оригинал)
	Skipped    bool
	RootTmp    string
	// PickerMsgs — все сообщения меню выбора обложки (по одному документу-
	// превью на картинку-кандидата + финальное сообщение с кнопками "Без
	// обложки"/"Загрузить свою", см. offerCoverSelection) — удаляются все
	// разом после выбора.
	PickerMsgs []*tele.Message

	// CueSplit — не nil, если это меню обложки не для обычных треков папки
	// (AudioPaths), а для целого альбома, который сейчас нарежут по
	// cue-sheet (см. offerCueCoverSelection в cue.go). В этом случае выбор
	// обложки завершается вызовом finishCueSplit, а не перебором AudioPaths.
	CueSplit *PendingCueSplit

	// Consumed — true, если решение по этой папке уже обрабатывается
	// (или обработано). Меню теперь может состоять из НЕСКОЛЬКИХ кнопок в
	// разных сообщениях (по одной на картинку-превью) — пока последние ещё
	// отправляются, первые уже кликабельны, и пользователь может успеть
	// тапнуть по двум разным кнопкам подряд, думая, что предыдущий тап не
	// засчитался. Без этой защиты второй тап повторно запускал бы
	// finishCueSplit/доставку треков и лишний раз декрементировал бы
	// счётчик аудио-задач (см. audioTaskCounts в этом файле).
	Consumed bool

	mu sync.Mutex // защищает поля структуры от гонок между потоком
	// закачки (tgbot/torr/manager.go) и обработчиками колбэков Telegram

	// compressOnce гарантирует, что сжатие выбранной обложки (до 48 вызовов
	// ffmpeg перебором качества/размера, см. compressCoverForEmbed) выполняется
	// РОВНО ОДИН РАЗ на папку, а не на каждый трек — результат переиспользуется
	// для всех треков этой папки.
	compressOnce         sync.Once
	compressedCoverBytes []byte
	compressErr          error
}

// getCompressedCoverBytes сжимает обложку coverPath не более одного раза за
// время жизни PendingCover; повторные вызовы отдают закэшированный результат.
func (pc *PendingCover) getCompressedCoverBytes(coverPath string) ([]byte, error) {
	pc.compressOnce.Do(func() {
		pc.compressedCoverBytes, pc.compressErr = compressCoverBytes(coverPath)
	})
	return pc.compressedCoverBytes, pc.compressErr
}

// compressCoverBytes сжимает обложку до ≤200 КБ и возвращает готовые байты.
// При ошибке сжатия отдаёт оригинал без изменений (тот же fallback, что был
// раньше в applyCoverToFile).
func compressCoverBytes(coverPath string) ([]byte, error) {
	compressedPath, err := compressCoverForEmbed(coverPath)
	if err != nil {
		log.Printf("[audio] не удалось сжать обложку (%v), используем оригинал без сжатия", err)
		return os.ReadFile(coverPath)
	}
	defer os.Remove(compressedPath)
	return os.ReadFile(compressedPath)
}

var pendingCovers sync.Map
var uploadExpect sync.Map

type uploadInfo struct {
	Hash    string
	DirHash string
}

// ---------------------------------------------------------------------
// Учёт незавершённых задач по временной папке торрента (rootTmp).
//
// Проблема, которую это решает: временную папку нельзя удалять ни когда
// пользователь выбрал обложку для первого трека (остальные треки и
// НЕаудио-файлы — документы, фото и т.п. — ещё выгружаются из этой же
// папки), ни просто в конце цикла выгрузки (треки могут всё ещё ждать,
// пока пользователь выберет обложку).
//
// Схема:
//  1. manager.go в начале закачки регистрирует ОДНУ "задачу-стража"
//     (RegisterAudioTasks(tmpDir, 1)) и закрывает её в самом конце
//     цикла выгрузки (CompleteAudioTask). Пока цикл не завершён,
//     счётчик гарантированно > 0 и папка не удаляется — значит,
//     неаудио-файлы спокойно выгружаются.
//  2. Перед КАЖДОЙ передачей аудиофайла в ProcessAudioFile manager.go
//     вызывает AddAudioTask(tmpDir) (+1). Задача закрывается, когда
//     трек реально отправлен пользователю (в т.ч. после выбора обложки).
//
// Папка удаляется, когда счётчик доходит до нуля — то есть когда
// завершён и цикл выгрузки, и все интерактивные аудиозадачи. Такой
// инкрементальный учёт также корректно работает с файлами из кэша
// Telegram (для них AddAudioTask просто не вызывается).
// ---------------------------------------------------------------------

// audioTaskEntry — счётчик незавершённых аудио-задач для одной временной
// папки плюс колбэк, который нужно вызвать РОВНО ОДИН раз, когда счётчик
// дойдёт до нуля — то есть когда действительно всё готово (а не когда
// manager.go просто передал последний физический файл конвейеру, что для
// cue-нарезаемых файлов означает только "меню нарезки показано"). count и
// onDone собираются в готовый объект ДО публикации в audioTaskCounts —
// как и раньше с голым *int64, доп. блокировка не нужна: onDone больше не
// меняется после Store.
//
// pendingBytes/onBytes — та же проблема, что решает onDone, но для
// прогресс-бара выгрузки, а не финального сообщения: manager.go раньше
// засчитывал file.Length в wrk.uploadedBytes сразу как только
// uploadFileFromDisk возвращался, но для файлов, отданных в
// AudioProcessor, "возврат" означает лишь "меню показано" — реальная
// отправка (в т.ч. всех треков cue-нарезки) происходит позже, асинхронно.
// AddAudioTask теперь принимает bytes и кладёт его в pendingBytes (FIFO);
// completeAudioTask, закрывая ЛЮБУЮ из задач папки, забирает ОДНО значение
// из очереди и передаёт в onBytes. Порядок пар AddAudioTask/completeAudioTask
// 1:1 гарантирован (см. manager.go), но какое именно значение спарится с
// каким конкретным закрытием — не важно: нас интересует только сумма.
// onProgress — колбэк для промежуточного статуса ДОЛГИХ интерактивных задач
// (сейчас — только cue-нарезка, см. performCueSplitWithCover в cue.go):
// между тем как файл докачался (100%) и тем как onDone наконец сработает,
// может пройти несколько минут (нарезка трек за треком) — без этого статус-
// сообщение всё это время просто висело неизменным. В отличие от onDone/
// onBytes вызывается МНОГО раз за жизнь задачи, не только на закрытии.
type audioTaskEntry struct {
	count      atomic.Int64
	onDone     func()
	onBytes    func(int64)
	onProgress func(string)

	bytesMu      sync.Mutex
	pendingBytes []int64
}

var audioTaskCounts sync.Map // rootTmp string -> *audioTaskEntry

// audioFolderCounts считает, сколько РАЗНЫХ папок-альбомов уже встретилось
// в рамках одной задачи (rootTmp) — используется только чтобы решить,
// нужен ли разделитель между альбомами в дискографии (см.
// maybeSendAlbumSeparator): для первой папки в задаче он не нужен (она и
// так единственная на тот момент), для второй и далее — нужен. Атомарный
// инкремент через LoadOrStore+Add — файлы из разных папок могут дойти до
// "новой папки" одновременно (конвейер обрабатывает несколько файлов
// параллельно).
var audioFolderCounts sync.Map // rootTmp string -> *int64

// maybeSendAlbumSeparator отправляет короткое сообщение с названием папки
// перед началом обработки НЕ первого альбома в дискографии (несколько
// папок-альбомов в одной раздаче) — чтобы в чате было видно, где
// заканчивается один альбом и начинается следующий. Для первой папки в
// задаче ничего не отправляет.
func maybeSendAlbumSeparator(c tele.Context, rootTmp, audioDir string) {
	if rootTmp == "" {
		return
	}
	counterVal, _ := audioFolderCounts.LoadOrStore(rootTmp, new(int64))
	n := atomic.AddInt64(counterVal.(*int64), 1)
	if n <= 1 {
		return
	}
	folderName := filepath.Base(audioDir)
	if _, err := c.Bot().Send(c.Recipient(), "💿 <b>"+folderName+"</b>", tele.ModeHTML); err != nil {
		log.Printf("[audio] не удалось отправить разделитель альбома %q: %v", folderName, err)
	}
}

// RegisterAudioTasks создаёт счётчик задач для временной папки торрента.
// Вызывается из manager.go один раз, до начала закачки, с count=1
// (задача-страж цикла выгрузки). onDone вызывается РОВНО ОДИН раз, когда
// счётчик дойдёт до нуля — manager.go передаёт сюда показ финального
// сообщения "✅ Отправлено...", вместо того чтобы показывать его сразу
// после того как конвейер передал последний файл (раньше это означало
// ложное "готово" для cue-нарезаемых альбомов, чья интерактивная обработка
// в этот момент только начиналась).
func RegisterAudioTasks(rootTmp string, count int, onDone func(), onBytes func(int64), onProgress func(string)) {
	if rootTmp == "" || count <= 0 {
		return
	}
	entry := &audioTaskEntry{onDone: onDone, onBytes: onBytes, onProgress: onProgress}
	entry.count.Store(int64(count))
	audioTaskCounts.Store(rootTmp, entry)
}

// UpdateAudioProgress передаёт промежуточный статус долгой интерактивной
// задачи (сейчас — только cue-нарезка) в статус-сообщение задачи — см.
// audioTaskEntry.onProgress. Не делает ничего, если для этой rootTmp нет
// зарегистрированной задачи (аудио-обработка отключена) или onProgress не
// задан.
func UpdateAudioProgress(rootTmp, text string) {
	if rootTmp == "" {
		return
	}
	if val, ok := audioTaskCounts.Load(rootTmp); ok {
		if onProgress := val.(*audioTaskEntry).onProgress; onProgress != nil {
			onProgress(text)
		}
	}
}

// AddAudioTask увеличивает счётчик задач на единицу. Вызывается из
// manager.go непосредственно перед передачей аудиофайла в ProcessAudioFile.
// bytes — размер ИМЕННО этого физического файла (file.Length в
// manager.go) — будет зачтён в wrk.uploadedBytes через onBytes, когда
// задача закроется (см. audioTaskEntry).
func AddAudioTask(rootTmp string, bytes int64) {
	if rootTmp == "" {
		return
	}
	if val, ok := audioTaskCounts.Load(rootTmp); ok {
		entry := val.(*audioTaskEntry)
		entry.count.Add(1)
		entry.bytesMu.Lock()
		entry.pendingBytes = append(entry.pendingBytes, bytes)
		entry.bytesMu.Unlock()
	}
}

// CompleteAudioTask закрывает одну задачу (экспортированная версия для
// manager.go — закрытие задачи-стража в конце цикла выгрузки).
func CompleteAudioTask(rootTmp string) {
	completeAudioTask(rootTmp)
}

func completeAudioTask(rootTmp string) {
	if rootTmp == "" {
		return
	}
	val, ok := audioTaskCounts.Load(rootTmp)
	if !ok {
		return
	}
	entry := val.(*audioTaskEntry)

	// Забираем ОДНО значение из очереди pendingBytes (если есть — задача-
	// страж, закрывающаяся без парного AddAudioTask, очередь не пополняет)
	// и зачисляем его в прогресс выгрузки. Делаем это на КАЖДОМ закрытии
	// задачи, а не только на последнем — каждая из них соответствует
	// одному реально отправленному физическому файлу.
	entry.bytesMu.Lock()
	var credited int64
	if len(entry.pendingBytes) > 0 {
		credited = entry.pendingBytes[0]
		entry.pendingBytes = entry.pendingBytes[1:]
	}
	onBytes := entry.onBytes
	entry.bytesMu.Unlock()
	if onBytes != nil && credited > 0 {
		onBytes(credited)
	}

	remaining := entry.count.Add(-1)
	if remaining > 0 {
		return
	}

	audioTaskCounts.Delete(rootTmp)
	audioFolderCounts.Delete(rootTmp)
	os.RemoveAll(rootTmp)

	var keysToDelete []interface{}
	pendingCovers.Range(func(key, value interface{}) bool {
		pc := value.(*PendingCover)
		if pc.RootTmp == rootTmp {
			keysToDelete = append(keysToDelete, key)
		}
		return true
	})
	for _, k := range keysToDelete {
		pendingCovers.Delete(k)
	}

	if entry.onDone != nil {
		entry.onDone()
	}
}

// ProcessAudioFile — точка входа AudioProcessor. oversized=true означает,
// что manager.go пропустил файл сюда В ОБХОД порога safePartSize только
// потому, что рядом нашёлся .cue (см. isCueSplitCandidate/hasSiblingCueFile
// в tgbot/torr/cue.go) — если по итогу нарезка не состоится (cue не
// разобрался/пользователь отказался), файл всё равно превышает лимит
// одиночной отправки, и нужно откатиться на fallback (7z-архивация через
// LargeFileProcessor). Когда oversized=false, fallback всегда nil.
func ProcessAudioFile(c tele.Context, filePath string, hash string, rootTmp string, fileID int, oversized bool, fallback func() error) error {
	ext := strings.ToLower(filepath.Ext(filePath))
	if ext != ".mp3" && ext != ".flac" && ext != ".m4a" && ext != ".ogg" {
		// Задача была зарегистрирована manager'ом до вызова — закрываем,
		// иначе счётчик никогда не дойдёт до нуля.
		completeAudioTask(rootTmp)
		return nil
	}

	// Cue-sheet проверяем ДО конвертации в M4A: если пользователь подтвердит
	// нарезку, резать (и заодно перекодировать под Bot API) нужно из
	// исходного FLAC по кускам, а не из уже сконвертированного целиком
	// файла. Сопроводительные .cue на диск кладёт prefetchCueSheets в
	// tgbot/torr/cue.go — до вызова ProcessAudioFile. В папке может быть
	// несколько .cue (или один общий на несколько файлов, см.
	// CueFileSection) — перебираем все, пока какой-то не подойдёт именно
	// этому файлу.
	var cueMeta *CueTrackMeta
	if ext == ".flac" {
		for _, cuePath := range findSiblingCueFiles(filePath) {
			handled, meta, err := offerCueSplit(c, filePath, cuePath, hash, rootTmp, fileID, oversized, fallback)
			if handled {
				return err
			}
			if meta != nil {
				cueMeta = meta
			}
			// этот .cue не описывает данный файл/не разобрался — пробуем
			// следующий, а если это был последний — обычный путь ниже.
		}
	}

	if oversized {
		// Не оказался настоящим cue-кандидатом, но превышает лимит
		// одиночной отправки — уходим в 7z-архивацию тем же путём, что и
		// раньше (до того как manager.go пропустил файл сюда в обход
		// порога). Порядок важен: сначала fallback (файл на диске ещё
		// должен существовать), потом completeAudioTask — иначе счётчик
		// может дойти до нуля и папка удалится до того, как архиватор
		// прочитает файл.
		err := fallback()
		completeAudioTask(rootTmp)
		return err
	}

	return processAudioFileNormally(c, filePath, hash, rootTmp, fileID, cueMeta)
}

// audioCacheKey — ключ кэша Telegram file_id (db.SaveTGFileID/GetTGFileID)
// для целого файла. Формат совпадает с тем, что уже использует
// tgbot/torr/manager.go (sendWithRetry/sendCachedFile) — единое пространство
// ключей, чтобы кэш, заполненный одним путём отправки (обычным файлом), был
// виден и другому (через AudioProcessor), и наоборот.
func audioCacheKey(hash string, fileID int) string {
	return fmt.Sprintf("%s|%d", hash, fileID)
}

// processAudioFileNormally — путь одиночного трека (без нарезки по cue):
// конвертация FLAC->M4A, определение обложки, отправка. Вызывается как из
// ProcessAudioFile напрямую (cue не найден или описывает этот файл одним
// треком — тогда cueMeta несёт его PERFORMER/TITLE, см. CueTrackMeta), так
// и из handleCueSplitDecline/applyCueGroupDecision, когда пользователь
// отказался от нарезки (там cueMeta всегда nil — секция с >=2 треками не
// сводится к одному названию).
func processAudioFileNormally(c tele.Context, filePath string, hash string, rootTmp string, fileID int, cueMeta *CueTrackMeta) error {
	cacheKey := audioCacheKey(hash, fileID)

	// Проверяем вшитую обложку в ИСХОДНОМ файле — до конвертации, потому
	// что convertToM4A (-vn) выбрасывает встроенную картинку. Сама
	// конвертация, как и выбор между юзерботом (для FLAC) и Bot API,
	// теперь откладывается до момента реальной отправки (см. deliverTrack)
	// — раньше юзербот-путь пробовался ЗДЕСЬ, до выбора обложки, и молча
	// забирал файл автовыбранной обложкой (cueAlbumCover) в обход всего
	// меню ниже.
	artist, title, duration, hasCover, coverData := readAudioInfo(filePath)
	// cue-sheet (если есть) — более надёжный источник PERFORMER/TITLE, чем
	// угадывание по имени файла внутри readAudioInfo: используем его, только
	// если самого тега в файле не было (embedded-тег имеет приоритет).
	if cueMeta != nil {
		if artist == "" && cueMeta.Artist != "" {
			artist = cueMeta.Artist
		}
		if title == "" && cueMeta.Title != "" {
			title = cueMeta.Title
		}
	}
	log.Printf("[audio] %s: artist=%q title=%q duration=%v hasCover=%v coverBytes=%d", filePath, artist, title, duration, hasCover, len(coverData))

	chatID := c.Sender().ID
	audioDir := filepath.Dir(filePath)
	dirHash := fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(audioDir)))
	key := fmt.Sprintf("%d_%s_%s", chatID, hash, dirHash)

	track := queuedTrack{Path: filePath, Artist: artist, Title: title, Duration: duration, CacheKey: cacheKey}

	// Картинки (включая вшитую в САМ этот файл обложку — см. ниже) готовим
	// ДО попытки застолбить запись в pendingCovers, а не после: конвейер
	// (см. pipelineConcurrency в manager.go) обрабатывает несколько файлов
	// ОДНОЙ папки параллельно, и несколько треков могут одновременно
	// увидеть "новой папки ещё нет" — без атомарности между проверкой и
	// записью (раньше здесь был отдельный Load, а Store — уже после
	// подготовки картинок) второй трек создавал СВОЙ PendingCover и
	// перезаписывал им первый в map, вместе с которым бесследно терялся
	// уже добавленный туда первый трек. Из-за этого и терялись отдельные
	// треки в альбомах без cue — см. историю чата. LoadOrStore ниже делает
	// "проверить или застолбить" одной атомарной операцией.
	images := findImagesInDir(audioDir)
	if hasCover && len(coverData) > 0 {
		if embeddedPath, err := saveEmbeddedCoverOption(audioDir, coverData); err == nil {
			images = append([]string{embeddedPath}, images...)
		} else {
			log.Printf("[audio] %s: не удалось сохранить вшитую обложку для меню: %v", filePath, err)
		}
	}

	newPC := &PendingCover{
		AudioPaths: []queuedTrack{track},
		AudioDir:   audioDir,
		Hash:       hash,
		Images:     images,
		RootTmp:    rootTmp,
	}
	actual, loaded := pendingCovers.LoadOrStore(key, newPC)
	pc := actual.(*PendingCover)

	if loaded {
		// Кто-то другой (другой трек этой же папки, обрабатываемый
		// конкурентно) застолбил запись первым — картинки, подготовленные
		// выше, просто не пригодились (embeddedPath из saveEmbeddedCoverOption
		// останется неиспользованным файлом в audioDir, но уйдёт вместе со
		// всей tmpDir задачи, как и остальные — не утечка).
		pc.mu.Lock()
		decided := pc.Skipped || pc.Selected != ""
		if !decided {
			pc.AudioPaths = append(pc.AudioPaths, track)
		}
		pc.mu.Unlock()

		if decided {
			log.Printf("[audio] %s: решение по обложке папки уже принято, обрабатываем сразу", filePath)
			return finishAudioProcessing(c, pc, track, rootTmp)
		}
		// Решение по обложке для этой папки ещё не принято — трек встал
		// в очередь и будет обработан вместе с остальными, когда
		// пользователь ответит. Задача закроется в обработчике колбэка.
		log.Printf("[audio] %s: решение по обложке ещё не принято, трек поставлен в очередь", filePath)
		return nil
	}

	log.Printf("[audio] %s: новая папка %s, найдено картинок=%d", filePath, audioDir, len(images))
	maybeSendAlbumSeparator(c, rootTmp, audioDir)

	var err error
	if len(images) > 0 {
		err = offerCoverSelection(c, hash, images, dirHash, audioDir)
	} else {
		err = requestCustomCover(c, hash, dirHash, audioDir)
	}
	if err != nil {
		log.Printf("[audio] %s: не удалось показать меню выбора обложки: %v", filePath, err)
		// Меню показать не удалось — пользователь не сможет ответить.
		// Убираем запись и закрываем задачу, чтобы не было утечки.
		pendingCovers.Delete(key)
		completeAudioTask(rootTmp)
	}
	return err
}

// finishAudioProcessing обрабатывает один трек, для папки которого решение
// по обложке уже принято.
func finishAudioProcessing(c tele.Context, pc *PendingCover, track queuedTrack, rootTmp string) error {
	pc.mu.Lock()
	selected := pc.Selected
	pc.mu.Unlock()

	err := deliverTrack(c, pc, track, selected)
	completeAudioTask(rootTmp)
	return err
}

// deliverTrack отправляет один трек с уже принятым решением по обложке
// (coverPath == "" значит "без обложки"). Общая точка для всех путей,
// которыми может завершиться выбор обложки папки (см. finishAudioProcessing,
// handleCoverSelection, handleCoverSkip, handleCustomCoverUpload).
// Инкапсулирует порядок "сначала пробуем юзербота без перекодирования для
// FLAC, конвертируем в M4A только если он недоступен или откатился" — этот
// выбор раньше делался ДО выбора обложки (см. processAudioFileNormally),
// из-за чего пользователь для FLAC вообще не видел меню.
func deliverTrack(c tele.Context, pc *PendingCover, track queuedTrack, coverPath string) error {
	ext := strings.ToLower(filepath.Ext(track.Path))

	if ext == ".flac" && userbot.Ready() {
		var coverBytes []byte
		if coverPath != "" {
			if pc != nil {
				coverBytes, _ = pc.getCompressedCoverBytes(coverPath)
			} else {
				coverBytes, _ = compressCoverBytes(coverPath)
			}
		}
		if trySendFlacViaUserbotWithCover(c, track.Path, track.Artist, track.Title, track.Duration, coverBytes, track.CacheKey) {
			return nil
		}
	}

	filePath := track.Path
	if ext == ".flac" {
		log.Printf("[audio] %s: конвертация FLAC -> M4A начата", filePath)
		m4aPath, err := convertToM4A(filePath)
		if err != nil {
			log.Printf("[audio] %s: конвертация FLAC -> M4A не удалась: %v, отправляем как FLAC", filePath, err)
		} else {
			log.Printf("[audio] %s: конвертация FLAC -> M4A успешна -> %s", filePath, m4aPath)
			filePath = m4aPath
		}
	}

	if coverPath != "" {
		return applyCoverToFile(c, pc, filePath, coverPath, track.Artist, track.Title, track.Duration, track.CacheKey)
	}
	return sendAudio(c, filePath, track.Artist, track.Title, track.Duration, nil, track.CacheKey)
}

// saveEmbeddedCoverOption сохраняет обложку, уже вшитую в аудиофайл, как
// файл-кандидат в меню выбора обложки папки (см. offerCoverSelection —
// подпись кнопки берётся из filepath.Base). Кладём её ПРЯМО В audioDir —
// той же tmpDir задачи, что и остальные картинки папки. Чистится вместе со
// всей задачей (см. ownedByAudioProcessor в manager.go), отдельно удалять
// не нужно.
//
// Имя ОБЯЗАНО быть уникальным (через os.CreateTemp), а не фиксированным
// "Встроенная обложка.ext": эту функцию вызывает КАЖДЫЙ трек папки со
// своей вшитой обложкой, ещё до того как известно, чей PendingCover
// победит гонку за pendingCovers.LoadOrStore (см. processAudioFileNormally)
// — конвейер обрабатывает несколько треков одной папки конкурентно
// (pipelineConcurrency в manager.go), и с фиксированным именем несколько
// треков одновременно писали бы поверх ОДНОГО и того же файла, из-за чего
// показанная в меню "встроенная обложка" могла оказаться от чужого трека.
func saveEmbeddedCoverOption(audioDir string, coverData []byte) (string, error) {
	ext := ".jpg"
	if len(coverData) >= 8 && bytes.HasPrefix(coverData, []byte{0x89, 'P', 'N', 'G'}) {
		ext = ".png"
	}
	f, err := os.CreateTemp(audioDir, "Встроенная обложка *"+ext)
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err := f.Write(coverData); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	f.Close()
	return path, nil
}

func convertToM4A(flacPath string) (string, error) {
	// Заменяем расширение .flac на .m4a, а не добавляем ".m4a" поверх —
	// иначе получается двойное расширение "Track.flac.m4a", из-за которого
	// некоторые клиенты (в т.ч. Telegram) неверно определяют формат файла.
	m4aPath := strings.TrimSuffix(flacPath, filepath.Ext(flacPath)) + ".m4a"
	args := []string{
		"-i", flacPath,
		"-map", "0:a",
		"-c:a", "alac",
		"-movflags", "+faststart",
		"-vn",
		m4aPath,
	}
	start := time.Now()
	log.Printf("[ffmpeg] convertToM4A: exec ffmpeg %s", strings.Join(args, " "))
	cmd := exec.Command("ffmpeg", args...)
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	if err != nil {
		os.Remove(m4aPath)
		log.Printf("[ffmpeg] convertToM4A: FAILED after %v: %v, output: %s", elapsed, err, out)
		return "", fmt.Errorf("ffmpeg error: %v, output: %s", err, out)
	}
	log.Printf("[ffmpeg] convertToM4A: OK after %v -> %s", elapsed, m4aPath)
	return m4aPath, nil
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

// sideSuffixRe вырезает хвост вида "Side A"/"SideB"/"Side 1" из разобранного
// имени файла — это маркер стороны пластинки/диска (см. parseFileName),
// а не часть реального названия трека; если в файле всего один трек, для
// пользователя это просто мусор в конце тайтла.
var sideSuffixRe = regexp.MustCompile(`(?i)\s+side\s*[a-z0-9]+$`)

func parseFileName(filePath string) (artist, title string) {
	base := filepath.Base(filePath)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)

	// Некоторые релизы (особенно старые сканы/риппы) называют файлы с
	// подчёркиваниями вместо пробелов — "Artist_Name_-_Track_Title.flac".
	// Обычный разделитель " - " в таком имени не встретится ни разу, зато
	// встретится "_-_" — переключаемся на него и заодно заменяем оставшиеся
	// подчёркивания на пробелы для читаемости.
	sep := " - "
	if !strings.Contains(name, sep) && strings.Contains(name, "_-_") {
		sep = "_-_"
	}

	parts := strings.SplitN(name, sep, 2)
	if len(parts) == 2 {
		artist = strings.TrimSpace(strings.ReplaceAll(parts[0], "_", " "))
		title = strings.TrimSpace(strings.ReplaceAll(parts[1], "_", " "))
	} else {
		title = strings.TrimSpace(strings.ReplaceAll(name, "_", " "))
	}
	title = strings.TrimSpace(sideSuffixRe.ReplaceAllString(title, ""))
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
		log.Printf("[ffmpeg] ffprobe %s: FAILED: %v", filePath, err)
		return 0
	}
	var info struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		log.Printf("[ffmpeg] ffprobe %s: не удалось разобрать JSON: %v", filePath, err)
		return 0
	}
	d, _ := strconv.ParseFloat(info.Format.Duration, 64)
	return int(d)
}

// muxerForExt возвращает имя мьюксера ffmpeg (-f) по расширению файла.
// Нужно, потому что временный файл получает суффикс ".tagged" и ffmpeg
// не всегда может определить формат контейнера по такому имени.
func muxerForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".m4a", ".mp4":
		return "ipod"
	case ".mp3":
		return "mp3"
	case ".ogg":
		return "ogg"
	case ".flac":
		return "flac"
	default:
		return ""
	}
}

func writeAudioTags(filePath, artist, title, coverPath string) error {
	origExt := filepath.Ext(filePath)
	tmpFile := strings.TrimSuffix(filePath, origExt) + ".tagged" + origExt

	args := []string{
		"-i", filePath,
		"-i", coverPath,
		"-map", "0:a",
		"-map", "1:v",
		"-c", "copy",
		"-metadata", fmt.Sprintf("artist=%s", artist),
		"-metadata", fmt.Sprintf("title=%s", title),
		"-disposition:v", "attached_pic",
	}

	if muxer := muxerForExt(origExt); muxer != "" {
		args = append(args, "-f", muxer)
	}
	args = append(args, tmpFile)

	start := time.Now()
	log.Printf("[ffmpeg] writeAudioTags: exec ffmpeg %s", strings.Join(args, " "))
	cmd := exec.Command("ffmpeg", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmpFile)
		log.Printf("[ffmpeg] writeAudioTags: FAILED after %v: %v, output: %s", time.Since(start), err, out)
		return fmt.Errorf("ffmpeg error: %v, output: %s", err, out)
	}

	info, err := os.Stat(tmpFile)
	if err != nil || info.Size() == 0 {
		os.Remove(tmpFile)
		log.Printf("[ffmpeg] writeAudioTags: пустой/отсутствующий результат %s", tmpFile)
		return fmt.Errorf("ошибка при создании файла с тегами")
	}
	log.Printf("[ffmpeg] writeAudioTags: OK after %v -> %s (%d bytes)", time.Since(start), filePath, info.Size())

	if err := os.Rename(tmpFile, filePath); err != nil {
		return err
	}
	return nil
}

// compressCoverForEmbed сжимает обложку до ≤200 КБ в формате JPEG.
// Именно этот сжатый вариант вшивается в аудиофайл (а не оригинал) и
// используется как превью в Telegram — так и файлы компактнее, и
// гарантированно выполняется ограничение Telegram на размер thumbnail.
//
// Возвращает путь к временному jpg-файлу; вызывающий код должен удалить
// его сам после использования.
func compressCoverForEmbed(coverPath string) (string, error) {
	const maxBytes = 200 * 1024

	outFile, err := os.CreateTemp("", "cover_*.jpg")
	if err != nil {
		return "", err
	}
	outPath := outFile.Name()
	outFile.Close()

	sizes := []int{800, 640, 480, 320, 240, 160}
	qualities := []int{3, 5, 7, 9, 12, 16, 20, 25}

	start := time.Now()
	attempts := 0
	var lastErr error
	for _, size := range sizes {
		for _, q := range qualities {
			attempts++
			args := []string{
				"-y",
				"-i", coverPath,
				"-vf", fmt.Sprintf("scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease", size, size),
				"-vframes", "1",
				"-q:v", strconv.Itoa(q),
				outPath,
			}
			cmd := exec.Command("ffmpeg", args...)
			if out, err := cmd.CombinedOutput(); err != nil {
				lastErr = fmt.Errorf("ffmpeg error: %v, output: %s", err, out)
				log.Printf("[ffmpeg] compressCoverForEmbed: size=%d q=%d FAILED: %v", size, q, err)
				continue
			}
			info, err := os.Stat(outPath)
			if err != nil {
				lastErr = err
				continue
			}
			if info.Size() <= maxBytes {
				log.Printf("[ffmpeg] compressCoverForEmbed: OK after %d attempt(s), %v, size=%d q=%d -> %d bytes", attempts, time.Since(start), size, q, info.Size())
				return outPath, nil
			}
		}
	}

	// Не удалось уложиться в 200 КБ ни при одной комбинации — отдаём
	// последний (самый сжатый) результат, если он вообще был создан.
	if info, err := os.Stat(outPath); err == nil && info.Size() > 0 {
		log.Printf("[ffmpeg] compressCoverForEmbed: не удалось уложиться в %d байт за %d попыток (%v), отдаём последний результат %d bytes", maxBytes, attempts, time.Since(start), info.Size())
		return outPath, nil
	}
	os.Remove(outPath)
	log.Printf("[ffmpeg] compressCoverForEmbed: провалено полностью за %d попыток (%v): %v", attempts, time.Since(start), lastErr)
	if lastErr != nil {
		return "", lastErr
	}
	return "", fmt.Errorf("не удалось сжать обложку")
}

// trySendFlacViaUserbotWithCover пытается доставить FLAC без перекодирования:
// юзербот (MTProto) заливает файл в служебную релей-группу, а бот (Bot API)
// копирует сообщение оттуда в чат с пользователем — см. package-level
// комментарий tgbot/userbot/client.go про то, почему напрямую пользователю
// написать нельзя. coverBytes — уже выбранная пользователем обложка (или
// nil, если решил без обложки, см. deliverTrack) — раньше эта функция сама
// автовыбирала обложку (cueAlbumCover) ДО того, как пользователь вообще
// видел меню выбора. Возвращает false в любом случае, когда трек нужно
// отправлять обычным путём (юзербот/релей не готовы либо сама отправка не
// удалась) — вызывающая сторона тогда продолжает как раньше (convertToM4A +
// Bot API).
func trySendFlacViaUserbotWithCover(c tele.Context, filePath, artist, title string, duration int, coverBytes []byte, cacheKey string) bool {
	if !userbot.Ready() {
		return false
	}

	msgID, chatID, err := userbot.SendToRelay(context.Background(), filePath, title, artist, duration, coverBytes)
	if err != nil {
		log.Printf("[audio] %s: userbot.SendToRelay ошибка, откат на Bot API: %v", filePath, err)
		return false
	}
	sent, err := c.Bot().Copy(c.Recipient(), tele.StoredMessage{MessageID: strconv.Itoa(msgID), ChatID: chatID})
	if err != nil {
		log.Printf("[audio] %s: копирование из релея не удалось, откат на Bot API: %v", filePath, err)
		return false
	}
	if cacheKey != "" && sent != nil && sent.Audio != nil && sent.Audio.FileID != "" {
		db.SaveTGFileID(cacheKey, sent.Audio.FileID)
	}
	log.Printf("[audio] %s: отправлено через userbot+релей (MTProto, оригинальный FLAC, без конвертации)", filePath)
	return true
}

// maxAudioSendRetries — сколько раз повторить отправку трека при сетевой
// ошибке (в т.ч. EOF от локального Bot API сервера). Раньше отправка была
// одноразовой: в отличие от обычных файлов (см. sendWithRetry в
// tgbot/torr/manager.go), трек с тегами/обложкой уходил через sendAudio без
// единой попытки повтора, и любой транзиентный обрыв соединения превращал
// уже готовый к отправке файл в постоянную ошибку выгрузки.
const maxAudioSendRetries = 5

func sendAudio(c tele.Context, filePath, artist, title string, duration int, coverData []byte, cacheKey string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(filePath))

	audio := &tele.Audio{
		// Явно указываем имя файла — без этого Telegram/telebot может
		// подставить формат по умолчанию (mp3), даже если реально
		// отправляется m4a/flac/ogg.
		FileName:  filepath.Base(filePath),
		Title:     title,
		Performer: artist,
		Duration:  duration,
	}
	switch ext {
	case ".flac":
		audio.MIME = "audio/flac"
	case ".m4a", ".mp4":
		audio.MIME = "audio/mp4"
	case ".ogg":
		audio.MIME = "audio/ogg"
	case ".mp3":
		audio.MIME = "audio/mpeg"
	}

	var sendErr error
	for attempt := 1; attempt <= maxAudioSendRetries; attempt++ {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		audio.File = tele.FromReader(file)
		if len(coverData) > 0 {
			// Пересоздаём Reader на каждую попытку — bytes.Reader после
			// первой успешной/неудачной отправки уже вычитан до конца.
			audio.Thumbnail = &tele.Photo{
				File: tele.FromReader(bytes.NewReader(coverData)),
			}
		}

		start := time.Now()
		_, sendErr = c.Bot().Send(c.Recipient(), audio)
		if sendErr == nil {
			log.Printf("[audio] sendAudio %s: OK after %v (попытка %d/%d)", filePath, time.Since(start), attempt, maxAudioSendRetries)
			if cacheKey != "" && audio.FileID != "" {
				db.SaveTGFileID(cacheKey, audio.FileID)
			}
			return nil
		}

		delay := torr.FloodRetryDelay(sendErr, 5*time.Second)
		log.Printf("[audio] sendAudio %s: попытка %d/%d FAILED after %v: %v (ждём %v)", filePath, attempt, maxAudioSendRetries, time.Since(start), sendErr, delay)
		if attempt < maxAudioSendRetries {
			time.Sleep(delay)
		}
	}
	return sendErr
}

// findImagesInDir ищет в папке файлы-обложки. Поиск регистронезависимый
// (Cover.JPG, COVER.jpg и т.п.) и охватывает расширенный список форматов;
// ffmpeg умеет декодировать любой из них при сжатии/вшивании.
func findImagesInDir(dir string) []string {
	exts := []string{
		".jpg", ".jpeg", ".png", ".webp", ".bmp", ".gif",
		".tif", ".tiff", ".jfif", ".heic", ".heif", ".avif",
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var images []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		for _, want := range exts {
			if ext == want {
				images = append(images, filepath.Join(dir, e.Name()))
				break
			}
		}
	}
	return images
}

// applyCoverToFile вшивает обложку в аудиофайл и отправляет её же как превью
// в Telegram. Сжатие обложки (до ≤200 КБ, самая дорогая часть — до 48
// запусков ffmpeg перебором качества/размера) выполняется один раз на pc
// (папку) через getCompressedCoverBytes и переиспользуется для всех треков.
// pc может быть nil (одиночный трек с уже вшитой обложкой, вне потока выбора
// обложки папки) — тогда сжатие просто не кэшируется.
func applyCoverToFile(c tele.Context, pc *PendingCover, audioPath, coverPath, artist, title string, duration int, cacheKey string) error {
	var coverBytes []byte
	var err error
	if pc != nil {
		coverBytes, err = pc.getCompressedCoverBytes(coverPath)
	} else {
		coverBytes, err = compressCoverBytes(coverPath)
	}
	if err != nil || len(coverBytes) == 0 {
		if err != nil {
			log.Printf("[audio] %s: обложка недоступна (%v), отправляем без обложки", audioPath, err)
		}
		return sendAudio(c, audioPath, artist, title, duration, nil, cacheKey)
	}

	tmpCover, err := writeTempCoverFile(coverBytes)
	if err != nil {
		log.Printf("[audio] %s: не удалось записать временный файл обложки (%v), отправляем без обложки", audioPath, err)
		return sendAudio(c, audioPath, artist, title, duration, nil, cacheKey)
	}
	defer os.Remove(tmpCover)

	if err := writeAudioTags(audioPath, artist, title, tmpCover); err != nil {
		return c.Send("⚠️ Не удалось записать теги: " + err.Error())
	}

	return sendAudio(c, audioPath, artist, title, duration, coverBytes, cacheKey)
}

// writeTempCoverFile сохраняет уже сжатые байты обложки во временный jpg —
// нужен как путь для ffmpeg -i, но, в отличие от исходного сжатия, это
// быстрая операция записи на диск без повторного запуска ffmpeg.
func writeTempCoverFile(data []byte) (string, error) {
	f, err := os.CreateTemp("", "cover_apply_*.jpg")
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	f.Close()
	return path, nil
}

// coverButtonsPerRow — сколько кнопок выбора обложки помещать в один ряд
// таблицы (см. offerCoverSelection) — 2 достаточно широко для длинных имён
// файлов и не разъезжается на экране телефона.
const coverButtonsPerRow = 2

// offerCoverSelection показывает меню выбора обложки в два шага: сначала
// КАЖДАЯ картинка-кандидат отдельным сообщением-документом (без кнопок —
// просто посмотреть, как она выглядит), а затем ОДНО отдельное сообщение
// с таблицей кнопок выбора, каждая подписана именем файла — так видно,
// какая кнопка какой картинке соответствует, не заваливая при этом каждую
// картинку своей парой кнопок. Документом (не фото) — Telegram всё равно
// показывает превью-миниатюру для документа-картинки, а в обычном потоке
// (см. uploadFileFromDisk в tgbot/torr/manager.go) та же картинка уже была
// один раз показана как фото, в порядке торрента — это НЕ то же самое
// сообщение, а отдельный повторный показ специально для меню.
func offerCoverSelection(c tele.Context, hash string, images []string, dirHash, audioDir string) error {
	folderName := filepath.Base(audioDir)

	for i, img := range images {
		doc := &tele.Document{
			FileName: filepath.Base(img),
			Caption:  fmt.Sprintf("Вариант %d: %s", i+1, filepath.Base(img)),
			File:     tele.FromDisk(img),
		}
		sentMsg, err := c.Bot().Send(c.Recipient(), doc)
		if err != nil {
			log.Printf("[audio] не удалось показать превью обложки %q: %v", img, err)
			continue
		}
		storePickerMsg(c, hash, dirHash, sentMsg)
	}

	markup := &tele.ReplyMarkup{}
	var rows []tele.Row
	var rowBtns []tele.Btn
	for i, img := range images {
		rowBtns = append(rowBtns, markup.Data(filepath.Base(img), "\fcover", hash, strconv.Itoa(i), dirHash))
		if len(rowBtns) == coverButtonsPerRow {
			rows = append(rows, markup.Row(rowBtns...))
			rowBtns = nil
		}
	}
	if len(rowBtns) > 0 {
		rows = append(rows, markup.Row(rowBtns...))
	}
	rows = append(rows, markup.Row(
		markup.Data("▶️ Без обложки", "\fskip", hash, dirHash),
		markup.Data("📤 Загрузить свою", "\fcovup", hash, dirHash),
	))
	markup.Inline(rows...)

	msgText := fmt.Sprintf("🎵 Выберите обложку для папки <b>%s</b>:", folderName)
	sentMsg, err := c.Bot().Send(c.Recipient(), msgText, markup, tele.ModeHTML)
	if err != nil {
		return err
	}
	storePickerMsg(c, hash, dirHash, sentMsg)
	return nil
}

func requestCustomCover(c tele.Context, hash string, dirHash, audioDir string) error {
	folderName := filepath.Base(audioDir)

	markup := &tele.ReplyMarkup{}
	markup.Inline(
		markup.Row(markup.Data("▶️ Без обложки", "\fskip", hash, dirHash)),
		markup.Row(markup.Data("📤 Загрузить свою", "\fcovup", hash, dirHash)),
	)
	msgText := fmt.Sprintf("📎 В папке <b>%s</b> нет картинок. Выберите действие:", folderName)
	sentMsg, err := c.Bot().Send(c.Recipient(), msgText, markup, tele.ModeHTML)
	if err != nil {
		return err
	}
	storePickerMsg(c, hash, dirHash, sentMsg)
	return nil
}

func storePickerMsg(c tele.Context, hash, dirHash string, msg *tele.Message) {
	chatID := c.Sender().ID
	key := fmt.Sprintf("%d_%s_%s", chatID, hash, dirHash)
	if val, ok := pendingCovers.Load(key); ok {
		pc := val.(*PendingCover)
		pc.mu.Lock()
		pc.PickerMsgs = append(pc.PickerMsgs, msg)
		pc.mu.Unlock()
	}
}

func handleCoverSkip(c tele.Context, hash, dirHash string) error {
	chatID := c.Sender().ID
	key := fmt.Sprintf("%d_%s_%s", chatID, hash, dirHash)
	val, ok := pendingCovers.Load(key)
	if !ok {
		return c.Respond(&tele.CallbackResponse{Text: "Данные устарели", ShowAlert: true})
	}
	pc := val.(*PendingCover)

	pc.mu.Lock()
	if pc.Consumed {
		pc.mu.Unlock()
		return c.Respond(&tele.CallbackResponse{Text: "Уже обработано"})
	}
	pc.Consumed = true
	pc.Skipped = true
	pc.Selected = ""
	paths := pc.AudioPaths
	pc.AudioPaths = nil
	pickerMsgs := pc.PickerMsgs
	pc.mu.Unlock()

	for _, m := range pickerMsgs {
		c.Bot().Delete(m)
	}

	if pc.CueSplit != nil {
		err := finishCueSplit(c, pc.CueSplit, "")
		completeAudioTask(pc.CueSplit.RootTmp)
		return err
	}

	var lastErr error
	for _, track := range paths {
		if err := deliverTrack(c, pc, track, ""); err != nil {
			lastErr = err
		}
		completeAudioTask(pc.RootTmp)
	}
	return lastErr
}

func handleCoverSelection(c tele.Context, hash string, imgIndex int, dirHash string) error {
	chatID := c.Sender().ID
	key := fmt.Sprintf("%d_%s_%s", chatID, hash, dirHash)
	val, ok := pendingCovers.Load(key)
	if !ok {
		return c.Respond(&tele.CallbackResponse{Text: "Данные устарели", ShowAlert: true})
	}
	pc := val.(*PendingCover)
	if imgIndex < 0 || imgIndex >= len(pc.Images) {
		return c.Respond(&tele.CallbackResponse{Text: "Неверный выбор"})
	}
	coverPath := pc.Images[imgIndex]

	pc.mu.Lock()
	if pc.Consumed {
		pc.mu.Unlock()
		return c.Respond(&tele.CallbackResponse{Text: "Уже обработано"})
	}
	pc.Consumed = true
	pc.Selected = coverPath
	pc.Skipped = false
	paths := pc.AudioPaths
	pc.AudioPaths = nil
	pickerMsgs := pc.PickerMsgs
	pc.mu.Unlock()

	for _, m := range pickerMsgs {
		c.Bot().Delete(m)
	}

	if pc.CueSplit != nil {
		err := finishCueSplit(c, pc.CueSplit, coverPath)
		completeAudioTask(pc.CueSplit.RootTmp)
		return err
	}

	var lastErr error
	for _, track := range paths {
		if err := deliverTrack(c, pc, track, coverPath); err != nil {
			lastErr = err
		}
		completeAudioTask(pc.RootTmp)
	}
	return lastErr
}

func handleCustomCoverUpload(c tele.Context, hash, dirHash string, msg *tele.Message) error {
	chatID := c.Sender().ID
	key := fmt.Sprintf("%d_%s_%s", chatID, hash, dirHash)
	val, ok := pendingCovers.Load(key)
	if !ok {
		return c.Send("Данные устарели. Начните сначала.")
	}
	pc := val.(*PendingCover)

	var file io.ReadCloser
	var err error
	fileExt := ".jpg"
	if msg.Photo != nil {
		// Telegram всегда пережимает отправленные "фото" в JPEG.
		file, err = downloadTelegramFile(c.Bot(), &msg.Photo.File)
		fileExt = ".jpg"
	} else if msg.Document != nil {
		file, err = downloadTelegramFile(c.Bot(), &msg.Document.File)
		if e := strings.ToLower(filepath.Ext(msg.Document.FileName)); e != "" {
			fileExt = e
		}
	} else {
		return c.Send("Пожалуйста, отправьте изображение.")
	}
	if err != nil {
		return err
	}
	defer file.Close()

	// Расширение обязательно нужно для ffmpeg: файл без расширения
	// ("custom_cover.tmp") не всегда корректно определяется как картинка.
	tmpPath := filepath.Join(pc.AudioDir, "custom_cover"+fileExt)
	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, file)
	out.Close()
	if copyErr != nil {
		os.Remove(tmpPath)
		return copyErr
	}

	pc.mu.Lock()
	if pc.Consumed {
		pc.mu.Unlock()
		os.Remove(tmpPath)
		return c.Send("Уже обработано.")
	}
	pc.Consumed = true
	pc.Selected = tmpPath
	pc.Skipped = false
	paths := pc.AudioPaths
	pc.AudioPaths = nil
	pickerMsgs := pc.PickerMsgs
	pc.mu.Unlock()

	for _, m := range pickerMsgs {
		c.Bot().Delete(m)
	}

	if pc.CueSplit != nil {
		err := finishCueSplit(c, pc.CueSplit, tmpPath)
		completeAudioTask(pc.CueSplit.RootTmp)
		return err
	}

	var lastErr error
	for _, track := range paths {
		if e := deliverTrack(c, pc, track, tmpPath); e != nil {
			lastErr = e
		}
		completeAudioTask(pc.RootTmp)
	}
	return lastErr
}
