package userbot

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/unpack"
	"github.com/gotd/td/telegram/uploader"
)

// ErrNotReady — MTProto-соединение и/или релей-группа сейчас не готовы (см.
// Ready()): либо юзербот не сконфигурирован (нет API_ID/API_HASH), либо
// сессия не авторизована, либо временный обрыв связи (см. runForever в
// client.go), либо не удалось создать/загрузить релей-группу (relay.go).
var ErrNotReady = errors.New("userbot: клиент не готов")

// SendToRelay заливает filePath в служебную релей-группу (см. relay.go) без
// перекодирования — в отличие от Bot API (sendAudio принимает только
// .mp3/.m4a), у "сырого" MTProto такого ограничения нет. Возвращает id
// сообщения и Bot-API chat_id группы: вызывающая сторона (у неё есть доступ
// к Bot API через tele.Context) должна скопировать это сообщение оттуда в
// реальный чат с пользователем, например:
//
//	msgID, chatID, err := userbot.SendToRelay(ctx, path, title, performer, dur, thumb)
//	...
//	c.Bot().Copy(c.Recipient(), tele.StoredMessage{MessageID: strconv.Itoa(msgID), ChatID: chatID})
//
// thumbData — уже готовые (сжатые) байты обложки, может быть nil/пустым —
// тогда сообщение уйдёт без превью. Байты, а не путь к файлу: обложка и так
// уже есть в памяти на стороне вызывающего (см. cueAlbumCover в
// tgbot/cue.go), доп. временный файл не нужен — uploader умеет грузить и из
// []byte напрямую (FromBytes).
func SendToRelay(ctx context.Context, filePath, title, performer string, durationSec int, thumbData []byte) (msgID int, chatID int64, err error) {
	if !Ready() || client == nil {
		return 0, 0, ErrNotReady
	}

	api := client.API()
	up := uploader.NewUploader(api)

	file, err := up.FromPath(ctx, filePath)
	if err != nil {
		return 0, 0, fmt.Errorf("userbot: загрузка файла: %w", err)
	}

	doc := message.UploadedDocument(file).MIME("audio/flac")
	if len(thumbData) > 0 {
		if thumb, err := up.FromBytes(ctx, "cover.jpg", thumbData); err == nil {
			doc = doc.Thumb(thumb)
		}
		// Ошибку заливки превью не считаем фатальной — трек всё равно
		// нужно отправить, просто без картинки.
	}

	audio := doc.Audio().
		Title(title).
		Performer(performer).
		DurationSeconds(durationSec).
		Filename(filepath.Base(filePath))

	sender := message.NewSender(api)
	id, err := unpack.MessageID(sender.To(relayPeer()).Media(ctx, audio))
	if err != nil {
		return 0, 0, fmt.Errorf("userbot: отправка в релей: %w", err)
	}
	return id, RelayChatID(), nil
}
