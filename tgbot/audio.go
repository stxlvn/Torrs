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
	PickerMsg  *tele.Message // сообщение с кнопками — удаляется после выбора

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

var audioTaskCounts sync.Map // rootTmp string -> *int64

// RegisterAudioTasks создаёт счётчик задач для временной папки торрента.
// Вызывается из manager.go один раз, до начала закачки, с count=1
// (задача-страж цикла выгрузки).
func RegisterAudioTasks(rootTmp string, count int) {
	if rootTmp == "" || count <= 0 {
		return
	}
	n := int64(count)
	audioTaskCounts.Store(rootTmp, &n)
}

// AddAudioTask увеличивает счётчик задач на единицу. Вызывается из
// manager.go непосредственно перед передачей аудиофайла в ProcessAudioFile.
func AddAudioTask(rootTmp string) {
	if rootTmp == "" {
		return
	}
	if val, ok := audioTaskCounts.Load(rootTmp); ok {
		atomic.AddInt64(val.(*int64), 1)
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
	counter := val.(*int64)
	remaining := atomic.AddInt64(counter, -1)
	if remaining > 0 {
		return
	}

	audioTaskCounts.Delete(rootTmp)
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
	if ext == ".flac" {
		for _, cuePath := range findSiblingCueFiles(filePath) {
			handled, err := offerCueSplit(c, filePath, cuePath, hash, rootTmp, fileID, oversized, fallback)
			if handled {
				return err
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

	return processAudioFileNormally(c, filePath, hash, rootTmp, fileID)
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
// ProcessAudioFile напрямую (cue не найден), так и из
// handleCueSplitDecline, когда пользователь отказался от нарезки.
func processAudioFileNormally(c tele.Context, filePath string, hash string, rootTmp string, fileID int) error {
	ext := strings.ToLower(filepath.Ext(filePath))

	cacheKey := audioCacheKey(hash, fileID)

	// Проверяем вшитую обложку в ИСХОДНОМ файле — до конвертации,
	// потому что convertToM4A (-vn) выбрасывает встроенную картинку.
	artist, title, duration, hasCover, coverData := readAudioInfo(filePath)
	log.Printf("[audio] %s: artist=%q title=%q duration=%v hasCover=%v coverBytes=%d", filePath, artist, title, duration, hasCover, len(coverData))

	// Юзербот (MTProto, см. tgbot/userbot) отправляет ОРИГИНАЛЬНЫЙ FLAC без
	// перекодирования — Bot API для sendAudio принимает только .mp3/.m4a,
	// поэтому этот путь пробуем ДО convertToM4A. Если юзербот недоступен
	// (не поднят) или пользователь ещё не писал ему первым (обязательное
	// условие Telegram — юзербот не может написать первым), молча
	// откатываемся на прежний путь ниже.
	if ext == ".flac" && trySendFlacViaUserbot(c, filePath, artist, title, duration, cacheKey) {
		completeAudioTask(rootTmp)
		return nil
	}

	converted := false
	if ext == ".flac" {
		log.Printf("[audio] %s: конвертация FLAC -> M4A начата", filePath)
		m4aPath, err := convertToM4A(filePath)
		if err != nil {
			log.Printf("[audio] %s: конвертация FLAC -> M4A не удалась: %v, отправляем как FLAC", filePath, err)
		} else {
			log.Printf("[audio] %s: конвертация FLAC -> M4A успешна -> %s", filePath, m4aPath)
			// Не удаляем m4aPath через defer — отправка может произойти
			// асинхронно (после выбора обложки); файл лежит внутри rootTmp
			// и будет удалён вместе с папкой через completeAudioTask.
			filePath = m4aPath
			converted = true
		}
	}

	// Если обложка уже вшита в файл — меню выбора не показываем:
	// отправляем сразу. Для сконвертированного FLAC вшиваем её обратно
	// (конвертация её удалила), для остальных — только готовим превью.
	if hasCover && len(coverData) > 0 {
		log.Printf("[audio] %s: обложка уже вшита, отправляем без выбора (converted=%v)", filePath, converted)
		err := sendWithEmbeddedCover(c, filePath, artist, title, duration, coverData, converted, cacheKey)
		if err != nil {
			log.Printf("[audio] %s: sendWithEmbeddedCover ошибка: %v", filePath, err)
		}
		completeAudioTask(rootTmp)
		return err
	}

	chatID := c.Sender().ID
	audioDir := filepath.Dir(filePath)
	dirHash := fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(audioDir)))
	key := fmt.Sprintf("%d_%s_%s", chatID, hash, dirHash)

	track := queuedTrack{Path: filePath, Artist: artist, Title: title, Duration: duration, CacheKey: cacheKey}

	if val, ok := pendingCovers.Load(key); ok {
		pc := val.(*PendingCover)
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

	images := findImagesInDir(audioDir)
	log.Printf("[audio] %s: новая папка %s, найдено картинок=%d", filePath, audioDir, len(images))
	pc := &PendingCover{
		AudioPaths: []queuedTrack{track},
		AudioDir:   audioDir,
		Hash:       hash,
		Images:     images,
		RootTmp:    rootTmp,
	}
	pendingCovers.Store(key, pc)

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
	var err error
	pc.mu.Lock()
	selected := pc.Selected
	pc.mu.Unlock()

	if selected != "" {
		err = applyCoverToFile(c, pc, track.Path, selected, track.Artist, track.Title, track.Duration, track.CacheKey)
	} else {
		err = sendAudio(c, track.Path, track.Artist, track.Title, track.Duration, nil, track.CacheKey)
	}
	completeAudioTask(rootTmp)
	return err
}

// sendWithEmbeddedCover отправляет трек, у которого обложка уже была вшита
// в исходный файл. reembed=true означает, что файл был сконвертирован
// (FLAC -> M4A) и обложку нужно вшить заново, т.к. конвертация её удалила.
func sendWithEmbeddedCover(c tele.Context, filePath, artist, title string, duration int, coverData []byte, reembed bool, cacheKey string) error {
	coverPath, err := saveCoverDataToTemp(coverData)
	if err != nil {
		// Не смогли сохранить картинку во временный файл — отправляем
		// без превью (в не-сконвертированном файле обложка и так внутри).
		return sendAudio(c, filePath, artist, title, duration, nil, cacheKey)
	}
	defer os.Remove(coverPath)

	if reembed {
		// applyCoverToFile сам сожмёт до ≤200 КБ, вошьёт и подставит превью.
		return applyCoverToFile(c, nil, filePath, coverPath, artist, title, duration, cacheKey)
	}

	// Обложка уже внутри файла — готовим только превью ≤200 КБ.
	var thumb []byte
	if compressed, cErr := compressCoverForEmbed(coverPath); cErr == nil {
		thumb, _ = os.ReadFile(compressed)
		os.Remove(compressed)
	} else if len(coverData) <= 200*1024 {
		thumb = coverData
	}
	return sendAudio(c, filePath, artist, title, duration, thumb, cacheKey)
}

// saveCoverDataToTemp сохраняет байты вшитой обложки во временный файл с
// корректным расширением (jpg/png по сигнатуре) — ffmpeg надёжнее
// определяет формат входной картинки по расширению.
func saveCoverDataToTemp(data []byte) (string, error) {
	ext := ".jpg"
	if len(data) >= 8 && bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G'}) {
		ext = ".png"
	}
	f, err := os.CreateTemp("", "embcover_*"+ext)
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

func parseFileName(filePath string) (artist, title string) {
	base := filepath.Base(filePath)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	parts := strings.SplitN(name, " - ", 2)
	if len(parts) == 2 {
		artist = strings.TrimSpace(parts[0])
		title = strings.TrimSpace(parts[1])
	} else {
		title = strings.TrimSpace(name)
	}
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

// trySendFlacViaUserbot пытается доставить FLAC без перекодирования: юзербот
// (MTProto) заливает файл в служебную релей-группу, а бот (Bot API)
// копирует сообщение оттуда в чат с пользователем — см. package-level
// комментарий tgbot/userbot/client.go про то, почему напрямую пользователю
// написать нельзя. Возвращает false в любом случае, когда трек нужно
// отправлять обычным путём (юзербот/релей не готовы либо сама отправка не
// удалась) — вызывающая сторона тогда продолжает как раньше (convertToM4A +
// Bot API).
func trySendFlacViaUserbot(c tele.Context, filePath, artist, title string, duration int, cacheKey string) bool {
	if !userbot.Ready() {
		return false
	}

	thumb := cueAlbumCover(filePath)
	msgID, chatID, err := userbot.SendToRelay(context.Background(), filePath, title, artist, duration, thumb)
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

func offerCoverSelection(c tele.Context, hash string, images []string, dirHash, audioDir string) error {
	folderName := filepath.Base(audioDir)

	markup := &tele.ReplyMarkup{}
	var rows []tele.Row
	for i, img := range images {
		btn := markup.Data(filepath.Base(img), "\fcover", hash, strconv.Itoa(i), dirHash)
		rows = append(rows, markup.Row(btn))
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
		pc.PickerMsg = msg
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
	pc.Skipped = true
	pc.Selected = ""
	paths := pc.AudioPaths
	pc.AudioPaths = nil
	pickerMsg := pc.PickerMsg
	pc.mu.Unlock()

	if pickerMsg != nil {
		c.Bot().Delete(pickerMsg)
	}

	var lastErr error
	for _, track := range paths {
		if err := sendAudio(c, track.Path, track.Artist, track.Title, track.Duration, nil, track.CacheKey); err != nil {
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
	pc.Selected = coverPath
	pc.Skipped = false
	paths := pc.AudioPaths
	pc.AudioPaths = nil
	pickerMsg := pc.PickerMsg
	pc.mu.Unlock()

	if pickerMsg != nil {
		c.Bot().Delete(pickerMsg)
	}

	var lastErr error
	for _, track := range paths {
		if err := applyCoverToFile(c, pc, track.Path, coverPath, track.Artist, track.Title, track.Duration, track.CacheKey); err != nil {
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
	pc.Selected = tmpPath
	pc.Skipped = false
	paths := pc.AudioPaths
	pc.AudioPaths = nil
	pickerMsg := pc.PickerMsg
	pc.mu.Unlock()

	if pickerMsg != nil {
		c.Bot().Delete(pickerMsg)
	}

	var lastErr error
	for _, track := range paths {
		if e := applyCoverToFile(c, pc, track.Path, tmpPath, track.Artist, track.Title, track.Duration, track.CacheKey); e != nil {
			lastErr = e
		}
		completeAudioTask(pc.RootTmp)
	}
	return lastErr
}
