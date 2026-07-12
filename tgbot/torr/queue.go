package torr

import (
	"fmt"
	"github.com/dustin/go-humanize"
	tele "gopkg.in/telebot.v4"
	"strconv"
	"time"
)

type DLQueue struct {
	id        int
	c         tele.Context
	hash      string
	fileID    string
	fileName  string
	updateMsg *tele.Message
}

var (
	manager = &Manager{}
)

func Start() {
	manager.Start()
}

func ShowQueue(c tele.Context) error {
	msg := ""
	manager.queueLock.Lock()
	defer manager.queueLock.Unlock()
	if len(manager.queue) == 0 && len(manager.working) == 0 {
		return c.Send("Очередь пуста")
	}
	if len(manager.working) > 0 {
		msg += "Закачиваются:\n"
		i := 0
		for _, dlQueue := range manager.working {
			s := "#" + strconv.Itoa(i+1) + ": <code>" + dlQueue.torrentHash + "</code> (" + strconv.Itoa(len(dlQueue.fileIndices)) + " файлов)\n"
			if len(msg+s) > 1024 {
				c.Send(msg, tele.ModeHTML)
				msg = ""
			}
			msg += s
			i++
		}
		if len(msg) > 0 {
			c.Send(msg, tele.ModeHTML)
			msg = ""
		}
	}
	if len(manager.queue) > 0 {
		msg = "В очереди:\n"
		for i, dlQueue := range manager.queue {
			s := "#" + strconv.Itoa(i+1) + ": <code>" + dlQueue.torrentHash + "</code> (" + strconv.Itoa(len(dlQueue.fileIndices)) + " файлов)\n"
			if len(msg+s) > 1024 {
				c.Send(msg, tele.ModeHTML)
				msg = ""
			}
			msg += s
		}
		if len(msg) > 0 {
			c.Send(msg, tele.ModeHTML)
			msg = ""
		}
	}
	return nil
}

func AddRange(c tele.Context, hash string, from, to int) {
	manager.AddRange(c, hash, from, to)
}

func Cancel(id int) {
	manager.Cancel(id)
}

func updateLoadStatus(wrk *Worker, file *TorrFile, fi, fc int) {
	if wrk.msg == nil {
		return
	}
	ti, err := GetTorrentInfo(wrk.torrentHash)
	if err != nil {
		wrk.c.Bot().Edit(wrk.msg, "Ошибка при получении данных о торренте", tele.ModeHTML)
		return
	} else if wrk.isCancelled {
		wrk.c.Bot().Edit(wrk.msg, "Остановка...", tele.ModeHTML)
		return
	}

	wrk.c.Send(tele.UploadingVideo)
	if ti.DownloadSpeed == 0 {
		ti.DownloadSpeed = 1.0
	}

	wait := time.Duration(float64(file.Loaded())/ti.DownloadSpeed) * time.Second
	speed := humanize.Bytes(uint64(ti.DownloadSpeed)) + "/sec"
	peers := fmt.Sprintf("%v (%v/%v)", ti.ConnectedSeeders, ti.ActivePeers, ti.TotalPeers)

	// Прогресс текущего файла (от 0 до 1)
	filePercent := 0.0
	if file.size > 0 {
		filePercent = float64(file.offset) / float64(file.size)
	}
	if filePercent > 1.0 {
		filePercent = 1.0
	}

	// Глобальный прогресс по всем скачиваемым файлам (от 0 до 100%)
	globalPercent := (float64(fi-1) + filePercent) / float64(fc) * 100.0
	if globalPercent > 100.0 {
		globalPercent = 100.0
	}

	downloadedStr := humanize.Bytes(uint64(file.offset))
	totalStr := humanize.Bytes(uint64(file.size))

	// Формируем комбинированное сообщение
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

	// Кнопка отмены теперь всегда присутствует на этапе загрузки, даже при финализации
	torrKbd := &tele.ReplyMarkup{}
	torrKbd.Inline([]tele.Row{torrKbd.Row(torrKbd.Data("Отмена", "cancel", strconv.Itoa(wrk.id)))}...)
	wrk.c.Bot().Edit(wrk.msg, msg, torrKbd, tele.ModeHTML)
}
