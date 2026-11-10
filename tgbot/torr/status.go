package torr

import (
	"fmt"
	"html"
	"log"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/dustin/go-humanize"
	tele "gopkg.in/telebot.v4"
)

// statusView — всё, что показывает статус-сообщение задачи.
type statusView struct {
	Title, Hash           string
	FilesDone, FilesTotal int
	Total                 int64

	DownDone       int64
	DownFinalizing bool
	Speed, Peers   string
	ETA            string

	UpDone        int64
	UpWaiting     bool
	AudioStage    string
	DownloadPhase bool
}

func percentOf(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return min(float64(done)/float64(total)*100, 100)
}

func (v statusView) richHTML() string {
	var b strings.Builder
	downPct := percentOf(v.DownDone, v.Total)
	upPct := percentOf(v.UpDone, v.Total)
	total := humanize.Bytes(uint64(v.Total))

	b.WriteString("<h4>🚀 " + html.EscapeString(v.Title) + "</h4>")
	b.WriteString("<table bordered compact>")
	b.WriteString("<tr><th></th><th>Прогресс</th><th>Объём</th></tr>")

	downVol := humanize.Bytes(uint64(v.DownDone)) + " / " + total
	switch {
	case downPct >= 100:
		downVol = "✅ " + total
	case v.DownFinalizing:
		downVol = "⏳ финализация"
	}
	fmt.Fprintf(&b, "<tr><td>📥 Скачивание</td><td>%s %.1f%%</td><td>%s</td></tr>", GetProgressBar(downPct), downPct, downVol)

	upVol := humanize.Bytes(uint64(v.UpDone)) + " / " + total
	if v.UpWaiting {
		upVol = "⏳ ждёт файлов"
	}
	fmt.Fprintf(&b, "<tr><td>📤 Выгрузка</td><td>%s %.1f%%</td><td>%s</td></tr>", GetProgressBar(upPct), upPct, upVol)
	b.WriteString("</table>")

	var info []string
	if v.FilesTotal > 1 {
		info = append(info, fmt.Sprintf("📦 Файлы: <b>%d из %d</b>", v.FilesDone, v.FilesTotal))
	}
	if v.DownloadPhase && !v.DownFinalizing && downPct < 100 {
		info = append(info, "⚡ "+v.Speed, "👥 "+v.Peers, "⏱ Осталось: <b>"+v.ETA+"</b>")
	}
	if len(info) > 0 {
		b.WriteString("<p>" + strings.Join(info, " · ") + "</p>")
	}

	if v.AudioStage != "" {
		b.WriteString("<blockquote><b>🎛 Обработка аудио</b><br>" + strings.ReplaceAll(v.AudioStage, "\n", "<br>") + "</blockquote>")
	}
	b.WriteString("<footer><code>" + v.Hash + "</code></footer>")
	return b.String()
}

func (v statusView) classicHTML() string {
	downPct := percentOf(v.DownDone, v.Total)
	upPct := percentOf(v.UpDone, v.Total)
	total := humanize.Bytes(uint64(v.Total))

	msg := "🚀 <b>Обработка торрента...</b>\n\n"
	msg += "💿 <b>Название:</b> " + html.EscapeString(v.Title) + "\n"
	if v.FilesTotal > 1 {
		msg += "📦 <b>Файлы:</b> " + strconv.Itoa(v.FilesDone) + " из " + strconv.Itoa(v.FilesTotal) + "\n\n"
	} else {
		msg += "\n"
	}
	msg += "📥 <b>Скачивание на сервер:</b>\n"
	msg += fmt.Sprintf("Прогресс: [%s] %.2f%%\n", GetProgressBar(downPct), downPct)
	switch {
	case v.DownFinalizing:
		msg += "⏳ <i>Финализация файла...</i>\n\n"
	case downPct >= 100:
		msg += "✅ <i>Успешно завершено</i>\n\n"
	case v.DownloadPhase:
		msg += fmt.Sprintf("Данные: %s / %s\n", humanize.Bytes(uint64(v.DownDone)), total)
		msg += fmt.Sprintf("Скорость: %s | Пиры: %s\n", v.Speed, v.Peers)
		msg += fmt.Sprintf("Осталось: %s\n\n", v.ETA)
	default:
		msg += fmt.Sprintf("Данные: %s / %s\n\n", humanize.Bytes(uint64(v.DownDone)), total)
	}
	msg += "📤 <b>Выгрузка в Telegram:</b>\n"
	msg += fmt.Sprintf("Прогресс: [%s] %.2f%%\n", GetProgressBar(upPct), upPct)
	if v.UpWaiting {
		msg += "⏳ <i>Ожидание скачивания файлов...</i>\n\n"
	} else {
		msg += fmt.Sprintf("Данные: %s / %s\n\n", humanize.Bytes(uint64(v.UpDone)), total)
	}
	if v.AudioStage != "" {
		msg += "🎛 <b>Обработка аудио:</b>\n" + v.AudioStage + "\n\n"
	}
	msg += "⚙️ <code>" + v.Hash + "</code>"
	return msg
}

// richDisabled — Bot API отказал в rich-сообщении; дальше статус рисуется
// обычным HTML до перезапуска.
var richDisabled atomic.Bool

func cancelMarkupJSON(id int) map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]any{{{
		"text":          "❌ Отмена",
		"callback_data": "\fcancel|" + strconv.Itoa(id),
		"style":         string(tele.ButtonStyleDanger),
	}}}}
}

// editRich правит сообщение в rich-формате; false — надо рисовать обычным HTML.
func editRich(bot tele.API, msg *tele.Message, richHTML string, markup map[string]any) bool {
	if richDisabled.Load() || msg == nil || msg.Chat == nil {
		return false
	}
	payload := map[string]any{
		"chat_id":      msg.Chat.ID,
		"message_id":   msg.ID,
		"rich_message": map[string]string{"html": richHTML},
	}
	if markup != nil {
		payload["reply_markup"] = markup
	}
	_, err := bot.Raw("editMessageText", payload)
	if err == nil || strings.Contains(err.Error(), "message is not modified") {
		return true
	}
	if strings.Contains(err.Error(), "rich") || strings.Contains(err.Error(), "method not found") || strings.Contains(err.Error(), "can't parse") {
		richDisabled.Store(true)
		log.Printf("[manager] rich-статус недоступен, перехожу на обычный HTML: %v", err)
		return false
	}
	log.Printf("[manager] rich-статус: %v", err)
	return true
}

func (wrk *Worker) showStatus(v statusView) {
	if editRich(wrk.c.Bot(), wrk.msg, v.richHTML(), cancelMarkupJSON(wrk.id)) {
		return
	}
	torrKbd := &tele.ReplyMarkup{}
	torrKbd.Inline([]tele.Row{torrKbd.Row(cancelButton(torrKbd, wrk.id))}...)
	wrk.c.Bot().Edit(wrk.msg, v.classicHTML(), torrKbd, tele.ModeHTML)
}

func (wrk *Worker) baseStatusView(title string, filesDone, filesTotal int) statusView {
	stage, _ := wrk.audioStage.Load().(string)
	up := wrk.uploadedBytes.Load()
	return statusView{
		Title:      title,
		Hash:       wrk.torrentHash,
		FilesDone:  filesDone,
		FilesTotal: filesTotal,
		Total:      wrk.totalBytes,
		DownDone:   wrk.downloadedBytes.Load(),
		UpDone:     up,
		AudioStage: stage,
	}
}

func queueStatusRich(item queueStatusItem) string {
	return fmt.Sprintf("<h4>⏳ В очереди: позиция %d</h4><p>📦 Файлов к обработке: <b>%d</b></p><footer><code>%s</code></footer>",
		item.position, item.fileCount, item.torrentHash)
}

func sendRich(bot tele.API, chatID, richHTML string) bool {
	if richDisabled.Load() {
		return false
	}
	_, err := bot.Raw("sendRichMessage", map[string]any{
		"chat_id":      chatID,
		"rich_message": map[string]string{"html": richHTML},
	})
	if err != nil {
		log.Printf("[manager] sendRichMessage: %v", err)
		return false
	}
	return true
}

func (wrk *Worker) summaryRich(totalFiles int) string {
	var b strings.Builder
	b.WriteString("<h4>✅ " + html.EscapeString(wrk.ti.Title) + "</h4>")
	b.WriteString("<table bordered compact>")
	fmt.Fprintf(&b, "<tr><td>📦 Отправлено файлов</td><td>%d</td></tr>", totalFiles)
	fmt.Fprintf(&b, "<tr><td>💽 Объём</td><td>%s</td></tr>", humanize.Bytes(uint64(wrk.totalBytes)))
	if DriveMirrorActive != nil && DriveMirrorActive() {
		b.WriteString("<tr><td>💾 Google Drive</td><td>резервная копия сохранена</td></tr>")
	}
	b.WriteString("</table>")
	return b.String()
}
