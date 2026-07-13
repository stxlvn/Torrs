package tgbot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	tele "gopkg.in/telebot.v4"
	"torrsru/tgbot/gdrive"
	"torrsru/tgbot/torr"
	"torrsru/tgbot/userbot"
)

func Start(token, host string) error {
	fmt.Println("=== BOT VERSION 2026-07-12-final ===")
	botToken = token
	botAPIHost = host
	pref := tele.Settings{
		URL:       host,
		Token:     token,
		Poller:    &tele.LongPoller{Timeout: 5 * time.Minute},
		ParseMode: tele.ModeHTML,
		// 60 минут — слишком долго для по-настоящему зависшего соединения:
		// наблюдали случай, когда файл том архива уже дошёл до Telegram, а
		// HTTP-ответ клиенту так и не пришёл — бот час просидел бы с "0
		// кнопки отмены нет" и без ретрая. 35 минут даёт запас для реально
		// медленной, но живой передачи тома ~1.9 ГБ (при ~1.3 МБ/с это
		// ~25 минут) и при этом не даёт зависнуть намертво — обрыв уйдёт в
		// sendVolumeWithRetry/sendWithRetry и попытка повторится.
		Client: &http.Client{Timeout: 35 * time.Minute},
		OnError: func(err error, c tele.Context) {
			if c != nil && c.Sender() != nil {
				log.Printf("[bot] необработанная ошибка хендлера (user=%d): %v", c.Sender().ID, err)
			} else {
				log.Printf("[bot] необработанная ошибка хендлера: %v", err)
			}
		},
	}

	b, err := tele.NewBot(pref)
	if err != nil {
		return err
	}

	torr.AudioProcessor = ProcessAudioFile
	torr.LargeFileProcessor = ProcessLargeFile
	// Хуки учёта задач по временной папке торрента (см. комментарии в
	// tgbot/torr/manager.go и tgbot/audio.go). Все три должны быть
	// привязаны вместе — иначе manager.go перейдёт в резервный режим
	// и будет удалять временную папку сам, без учёта интерактивного
	// выбора обложек.
	torr.RegisterAudioTasks = RegisterAudioTasks
	torr.AddAudioTask = AddAudioTask
	torr.CompleteAudioTask = CompleteAudioTask

	_ = b.SetCommands([]tele.Command{
		{Text: "start", Description: "Начало работы и справка"},
		{Text: "help", Description: "Помощь по боту"},
		{Text: "queue", Description: "Посмотреть очередь загрузок"},
		{Text: "id", Description: "Узнать свой Telegram ID"},
	})

	_ = b.SetMyDescription("Бот для скачивания торрентов. Поддерживает magnet-ссылки, хеши, .torrent файлы. Умеет добавлять теги и обложки к аудио.", "ru")
	_ = b.SetMyShortDescription("Торрент-качалка с тегами и обложками", "ru")

	b.Handle("help", help)
	b.Handle("Help", help)
	b.Handle("/help", help)
	b.Handle("/Help", help)
	b.Handle("/start", help)
	b.Handle("/queue", torr.ShowQueue)

	b.Handle("/id", func(c tele.Context) error {
		return c.Send(fmt.Sprintf("%v %v %v %v", c.Sender().ID, c.Sender().Username, c.Sender().FirstName, c.Sender().LastName))
	})
	b.Handle("/exit", func(c tele.Context) error {
		if c.Sender().ID == 140045144 {
			c.Send("Exit")
			os.Exit(0)
		}
		return nil
	})

	b.Handle(tele.OnText, func(c tele.Context) error {
		txt := c.Text()
		if strings.HasPrefix(strings.ToLower(txt), "magnet:") || isHash(txt) {
			return infoTorrent(c, c.Text())
		} else if c.Message().ReplyTo != nil && c.Message().ReplyTo.ReplyMarkup != nil && len(c.Message().ReplyTo.ReplyMarkup.InlineKeyboard) > 0 {
			data := c.Message().ReplyTo.ReplyMarkup.InlineKeyboard[0][0].Data
			if strings.HasPrefix(strings.ToLower(data), "\fall|") {
				hash := strings.TrimPrefix(data, "\fall|")
				from, to, err := ParseRange(c.Message().Text)
				if err != nil {
					c.Send("Ошибка, нужно указывать числа, пример: 2-12")
					return err
				}
				torr.AddRange(c, hash, from, to)
			}
			return nil
		} else {
			return c.Send("Вставьте магнет-ссылку, хеш торрента или отправьте .torrent файл\n\nУчтите, что файл не должен превышать 2 ГБ — это лимит self-hosted Bot API сервера на отправку файлов")
		}
	})

	// Обработчик фото – обложка для аудио
	b.Handle(tele.OnPhoto, func(c tele.Context) error {
		if info, ok := uploadExpect.Load(c.Sender().ID); ok {
			inf := info.(uploadInfo)
			uploadExpect.Delete(c.Sender().ID)
			return handleCustomCoverUpload(c, inf.Hash, inf.DirHash, c.Message())
		}
		return nil
	})

	// Единый обработчик документов: .torrent и обложки
	b.Handle(tele.OnDocument, func(c tele.Context) error {
		doc := c.Message().Document
		if strings.HasSuffix(strings.ToLower(doc.FileName), ".torrent") {
			log.Printf("[bot] .torrent от user=%d: %s (%d bytes)", c.Sender().ID, doc.FileName, doc.FileSize)
			rc, err := downloadTelegramFile(b, &doc.File)
			if err != nil {
				log.Printf("[bot] .torrent %s: скачивание файла FAILED: %v", doc.FileName, err)
				return nil
			}
			defer rc.Close()
			meta, err := metainfo.Load(rc)
			if err != nil {
				log.Printf("[bot] .torrent %s: разбор metainfo FAILED: %v", doc.FileName, err)
				return c.Send("❌ Ошибка: битый торрент-файл.")
			}
			hash := meta.HashInfoBytes().HexString()
			log.Printf("[bot] .torrent %s: hash=%s", doc.FileName, hash)
			return infoTorrent(c, hash)
		}

		if info, ok := uploadExpect.Load(c.Sender().ID); ok {
			inf := info.(uploadInfo)
			uploadExpect.Delete(c.Sender().ID)
			return handleCustomCoverUpload(c, inf.Hash, inf.DirHash, c.Message())
		}
		return nil
	})

	// Универсальный обработчик callback
	b.Handle(tele.OnCallback, func(c tele.Context) error {
		args := c.Args()
		if len(args) > 0 {
			cmd := strings.TrimSpace(args[0])
			if cmd == "\ftorr" {
				return infoTorrent(c, args[1])
			}
			return getTorrent(c)
		}
		log.Printf("[bot] callback без аргументов от user=%d", c.Sender().ID)
		return errors.New("Ошибка кнопка не распознана")
	})

	torr.Start()
	// Юзербот (MTProto, см. tgbot/userbot) — второй, отдельный Telegram-
	// аккаунт для доставки FLAC без перекодирования в M4A (Bot API для
	// sendAudio принимает только .mp3/.m4a). Не блокирует и не мешает
	// работе, если не сконфигурирован (нет API_ID/API_HASH в окружении)
	// или сессия ещё не авторизована — тогда просто логирует это, и весь
	// FLAC продолжает идти прежним путём (см. trySendFlacViaUserbot в
	// tgbot/audio.go). Username бота нужен юзерботу один раз — добавить
	// его в служебную релей-группу (см. tgbot/userbot/relay.go).
	var botUsername string
	if b.Me != nil {
		botUsername = b.Me.Username
	}
	userbot.Start(context.Background(), botUsername)

	// Google Drive (см. tgbot/gdrive) — резервное зеркало скачанных файлов,
	// независимое от Telegram. Не блокирует и не мешает работе, если не
	// сконфигурирован (нет GDRIVE_CLIENT_ID/SECRET) или сессия ещё не
	// авторизована (см. cmd/gdrive-login) — тогда просто логирует это, и
	// файлы продолжают доставляться в Telegram как обычно, без бэкапа.
	gdrive.Start(context.Background())
	torr.DriveMirrorActive = gdrive.Ready
	torr.MirrorToDrive = func(torrentTitle, localPath string) {
		if !gdrive.Ready() {
			return
		}
		// Открываем СИНХРОННО (быстро — просто open()), пока файл
		// гарантированно ещё на месте: конвейер может удалить localPath
		// сразу после выгрузки в Telegram, но уже открытый дескриптор
		// это переживает. Саму заливку по сети уводим в фон, чтобы не
		// блокировать конвейер надолго.
		f, err := gdrive.OpenFile(localPath)
		if err != nil {
			log.Printf("[gdrive] бэкап файла %q (%s): %v", localPath, torrentTitle, err)
			return
		}
		go func() {
			release := gdrive.AcquireUploadSlot()
			defer release()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			if err := gdrive.UploadOpenFile(ctx, f, torrentTitle); err != nil {
				log.Printf("[gdrive] бэкап файла %q (%s) не удался: %v", localPath, torrentTitle, err)
			}
		}()
	}

	go b.Start()
	return nil
}

func help(c tele.Context) error {
	msg := "🤖 <b>Доступные команды:</b>\n" +
		"/start — начать работу\n" +
		"/help — эта справка\n" +
		"/queue — состояние очереди загрузок\n" +
		"/id — ваш Telegram ID\n\n" +
		"📥 <b>Как добавить торрент:</b>\n" +
		"1. Отправьте магнет-ссылку, хеш или загрузите .torrent файл\n" +
		"2. В меню выбора файлов отметьте нужные\n" +
		"3. Нажмите «📥 Скачать выбранное»\n\n" +
		"🎵 <b>Аудиофайлы:</b>\n" +
		"• Поддерживаются MP3, FLAC, M4A, OGG\n" +
		"• Бот автоматически добавит исполнителя и название\n" +
		"• Если обложка уже вшита в файл, она используется автоматически\n" +
		"• Если в торренте есть картинки, можно выбрать обложку\n" +
		"• Можно загрузить свою обложку\n" +
		"• Если альбом одним FLAC-файлом и рядом есть .cue — бот предложит нарезать на отдельные треки\n\n" +
		"📦 <b>Файлы больше 1.9 ГБ:</b>\n" +
		"• Автоматически разбиваются на тома 7-Zip\n" +
		"• После отправки всех частей вы получите инструкцию по распаковке"
	return c.Send(msg, tele.ModeHTML)
}

func ParseRange(rng string) (int, int, error) {
	parts := strings.Split(rng, "-")
	if len(parts) != 2 {
		return -1, -1, errors.New("Неверный формат строки")
	}
	num1, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err1 != nil {
		return -1, -1, err1
	}
	num2, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err2 != nil {
		return -1, -1, err2
	}
	return num1, num2, nil
}
