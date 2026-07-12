package torr

import (
	"strconv"

	tele "gopkg.in/telebot.v4"
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
