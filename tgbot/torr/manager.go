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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dhowden/tag"
	tele "gopkg.in/telebot.v4"
	"torrsru/db"
	"torrsru/tgbot/torr/state"
)

// AudioProcessor – если задана, вызывается для каждого аудиофайла после скачивания.
// Позволяет внешнему коду (tgbot) обработать файл (добавить теги, обложку).
var AudioProcessor func(c tele.Context, filePath string, hash string) error

type Worker struct {
	id          int
	c           tele.Context
	msg         *tele.Message
	torrentHash string
	isCancelled bool
	fileIndices []int
	ti          *state.TorrentStatus
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
	m.queueLock.Lock()
	defer m.queueLock.Unlock()

	if len(m.queue) > 50 {
		c.Bot().Send(c.Recipient(), "Очередь переполнена, попробуйте попозже\n\nЭлементов в очереди:"+strconv.Itoa(len(m.queue)))
		return
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
			}
		}

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

	ti, _ := GetTorrentInfo(hash)
	if ti == nil {
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
	for i := from - 1; i <= to-1; i++ {
		fileIndices = append(fileIndices, i)
	}

	w := &Worker{
		id:          m.ids,
		c:           c,
		torrentHash: hash,
		msg:         msg,
		ti:          ti,
		fileIndices: fileIndices,
	}

	m.queue = append(m.queue, w)
	m.sendQueueStatusLocked()
}

func (m *Manager) Cancel(id int) {
	m.queueLock.Lock()
	defer m.queueLock.Unlock()
	var rem []int
	for i, w := range m.queue {
		if w.id == id {
			w.isCancelled = true
			w.c.Bot().Delete(w.msg)
			rem = append(rem, i)
			return
		}
	}
	for _, i := range rem {
		m.queue = append(m.queue[:i], m.queue[i+1:]...)
	}
	if wrk, ok := m.working[id]; ok {
		wrk.isCancelled = true
		return
	}
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

		m.sendQueueStatus()

		loading(wrk)

		m.queueLock.Lock()
		delete(m.working, wrk.id)
		m.queueLock.Unlock()
	}
}

func (m *Manager) sendQueueStatus() {
	m.queueLock.Lock()
	defer m.queueLock.Unlock()
	m.sendQueueStatusLocked()
}

func (m *Manager) sendQueueStatusLocked() {
	for i, wrk := range m.queue {
		if wrk.msg == nil {
			continue
		}
		torrKbd := &tele.ReplyMarkup{}
		torrKbd.Inline([]tele.Row{torrKbd.Row(torrKbd.Data("Отмена", "cancel", strconv.Itoa(wrk.id)))}...)

		msg := "⏳ <b>Очередь</b> (позиция " + strconv.Itoa(i+1) + ")\n"
		msg += "📦 <b>Файлов к обработке:</b> " + strconv.Itoa(len(wrk.fileIndices)) + "\n"
		msg += "⚙️ <code>" + wrk.torrentHash + "</code>"

		wrk.c.Bot().Edit(wrk.msg, msg, torrKbd, tele.ModeHTML)
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
	iserr := false
	totalFiles := len(wrk.fileIndices)

	// ФАЗА 1: Скачивание
	for idx, fileIndex := range wrk.fileIndices {
		if wrk.isCancelled {
			return
		}
		file := wrk.ti.FileStats[fileIndex]

		err := downloadFileToServer(wrk, file, idx+1, totalFiles)
		if err != nil {
			errstr := fmt.Sprintf("Ошибка скачивания файла на сервер: %v\n\n%v", file.Path, err.Error())
			wrk.c.Bot().Edit(wrk.msg, errstr, tele.ModeHTML)
			iserr = true
			break
		}
	}

	if iserr || wrk.isCancelled {
		return
	}

	// ФАЗА 2: Выгрузка
	for idx, fileIndex := range wrk.fileIndices {
		if wrk.isCancelled {
			return
		}
		file := wrk.ti.FileStats[fileIndex]

		err := uploadFileToTG(wrk, file, idx+1, totalFiles)
		if err != nil {
			errstr := fmt.Sprintf("Ошибка выгрузки файла в телеграм: %v\n\n%v", file.Path, err.Error())
			wrk.c.Bot().Edit(wrk.msg, errstr, tele.ModeHTML)
			iserr = true
			break
		}
	}

	if !iserr && !wrk.isCancelled {
		wrk.c.Bot().Delete(wrk.msg)
	}
}

func downloadFileToServer(wrk *Worker, file *state.TorrentFileStat, fi, fc int) error {
	tgfid := db.GetTGFileID(wrk.torrentHash + "|" + strconv.Itoa(file.Id))
	if tgfid != "" {
		return nil
	}

	torrFileDownload, err := NewTorrFile(wrk, file)
	if err != nil {
		return err
	}

	var wa sync.WaitGroup
	wa.Add(1)
	complete := false

	go func() {
		for !complete {
			if wrk.isCancelled {
				complete = true
				break
			}
			updateLoadStatus(wrk, torrFileDownload, fi, fc)
			time.Sleep(1 * time.Second)
		}
		wa.Done()
	}()

	buf := make([]byte, 1024*1024)
	for {
		if wrk.isCancelled {
			break
		}
		_, readErr := torrFileDownload.Read(buf)
		if readErr != nil {
			break
		}
	}

	complete = true
	wa.Wait()
	torrFileDownload.Close()

	if wrk.isCancelled {
		return errors.New("скачивание отменено пользователем")
	}

	return nil
}

func uploadFileToTG(wrk *Worker, file *state.TorrentFileStat, fi, fc int) error {
	if wrk.isCancelled {
		return errors.New("выгрузка отменена пользователем")
	}

	caption := filepath.Base(file.Path)
	tgfid := db.GetTGFileID(wrk.torrentHash + "|" + strconv.Itoa(file.Id))

	percent := float64(fi-1) / float64(fc) * 100.0

	ti, _ := GetTorrentInfo(wrk.torrentHash)
	title := wrk.torrentHash
	if ti != nil && ti.Title != "" {
		title = ti.Title
	}

	msg := "🚀 <b>Обработка торрента...</b>\n\n"
	msg += "💿 <b>Название:</b> " + title + "\n"
	if fc > 1 {
		msg += "📦 <b>Файлы:</b> " + strconv.Itoa(fi) + " из " + strconv.Itoa(fc) + "\n\n"
	} else {
		msg += "\n"
	}

	msg += "📥 <b>Скачивание на сервер:</b>\n"
	msg += fmt.Sprintf("Прогресс: [%s] 100.00%%\n", GetProgressBar(100.0))
	msg += "✅ <i>Успешно завершено</i>\n\n"

	msg += "📤 <b>Выгрузка в Telegram:</b>\n"
	msg += fmt.Sprintf("Прогресс: [%s] %.2f%%\n", GetProgressBar(percent), percent)
	msg += fmt.Sprintf("🎵 Отправляется: <i>%s</i>\n\n", caption)

	msg += "⚙️ <code>" + wrk.torrentHash + "</code>"

	// Добавляем кнопку отмены для процесса выгрузки
	torrKbd := &tele.ReplyMarkup{}
	torrKbd.Inline([]tele.Row{torrKbd.Row(torrKbd.Data("Отмена", "cancel", strconv.Itoa(wrk.id)))}...)
	wrk.c.Bot().Edit(wrk.msg, msg, torrKbd, tele.ModeHTML)

	torrFileUpload, err := NewTorrFile(wrk, file)
	if err != nil {
		return err
	}
	defer torrFileUpload.Close()

	ext := strings.ToLower(filepath.Ext(file.Path))
	isAudio := ext == ".mp3" || ext == ".flac" || ext == ".m4a" || ext == ".wav" || ext == ".ogg" || ext == ".aac"

	// Если это аудио и задан AudioProcessor – сохраняем во временный файл и делегируем обработку
	if isAudio && AudioProcessor != nil {
		tmpFile, err := os.CreateTemp("", "torraudio_*"+ext)
		if err != nil {
			return err
		}
		tmpPath := tmpFile.Name()
		defer os.Remove(tmpPath)

		_, err = io.Copy(tmpFile, torrFileUpload)
		torrFileUpload.Close()
		tmpFile.Close()
		if err != nil {
			return err
		}
		// Передаём управление внешнему обработчику (tgbot.ProcessAudioFile)
		return AudioProcessor(wrk.c, tmpPath, wrk.torrentHash)
	}

	var sendable interface{}

	if isAudio {
		audio := &tele.Audio{
			FileName: file.Path,
			Caption:  caption,
		}

		if tgfid != "" {
			audio.FileID = tgfid
		} else {
			if rs, ok := interface{}(torrFileUpload).(io.ReadSeeker); ok {
				if m, err := tag.ReadFrom(rs); err == nil {
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
				rs.Seek(0, io.SeekStart)
			}
			audio.File.FileReader = torrFileUpload
		}
		sendable = audio
	} else {
		d := &tele.Document{
			FileName: file.Path,
			Caption:  caption,
		}
		if tgfid != "" {
			d.FileID = tgfid
		} else {
			d.File.FileReader = torrFileUpload
		}
		sendable = d
	}

	var sendErr error
	for i := 0; i < 20; i++ {
		if wrk.isCancelled {
			return errors.New("выгрузка отменена пользователем")
		}
		sendErr = wrk.c.Send(sendable)
		if sendErr == nil || errors.Is(sendErr, ERR_STOPPED) {
			break
		} else {
			log.Println("Error send msg, try again:", i+1, "/", 20)
			time.Sleep(2 * time.Second)
		}
	}

	if errors.Is(sendErr, ERR_STOPPED) {
		sendErr = nil
	} else if sendErr != nil {
		log.Println("Error send message:", sendErr)
	} else {
		var savedFileID string
		if isAudio {
			if a, ok := sendable.(*tele.Audio); ok {
				savedFileID = a.FileID
			}
		} else {
			if d, ok := sendable.(*tele.Document); ok {
				savedFileID = d.FileID
			}
		}
		if savedFileID != "" {
			db.SaveTGFileID(wrk.torrentHash+"|"+strconv.Itoa(file.Id), savedFileID)
		}
	}

	return sendErr
}
