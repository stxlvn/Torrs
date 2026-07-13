// Package userbot доставляет FLAC-файлы без перекодирования в обход
// ограничения Bot API (sendAudio официально принимает только .mp3/.m4a).
// Работает как отдельный, обычный Telegram-аккаунт ("юзербот"), постоянно
// подключённый параллельно с основным ботом (см. tgbot/bot.go).
//
// Юзербот не может написать пользователю напрямую в его личный чат с
// @torrs_bot — это чужой приватный чат, третьему аккаунту туда сообщения не
// отправить в принципе (ограничение Telegram, не связано с этим кодом), а
// если юзербот залогинен под тем же аккаунтом, что и тестирующий
// пользователь, прямая отправка "самому себе" вообще уходит в Saved
// Messages. Поэтому доставка идёт через релей (см. relay.go): юзербот
// кладёт оригинальный FLAC в служебную группу, а бот (Bot API, у него есть
// доступ и к группе, и к чату с пользователем) копирует сообщение оттуда
// пользователю — см. Sender.To(...).Media(...) в send.go и Copy(...) в
// вызывающем коде (tgbot/audio.go, tgbot/cue.go).
package userbot

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"

	"torrsru/global"
)

var (
	client *telegram.Client
	ready  atomic.Bool
)

// Ready сообщает, авторизован ли юзербот, поднято ли MTProto-соединение и
// готова ли релей-группа прямо сейчас. Пока false, вся отправка через
// юзербота невозможна — вызывающая сторона должна проверять это (или просто
// ловить ошибку) и откатываться на старый путь через Bot API.
func Ready() bool {
	return ready.Load() && RelayChatID() != 0
}

// SessionPath возвращает путь к файлу сессии — тот же, что использует
// cmd/userbot-login. USERBOT_SESSION переопределяет дефолт (рядом с
// бинарником, там же где torrents.db/index.db — см. .gitignore).
func SessionPath() string {
	if p := os.Getenv("USERBOT_SESSION"); p != "" {
		return p
	}
	return filepath.Join(global.PWD, "userbot.session")
}

// Start поднимает MTProto-клиент юзербота в фоне (не блокирует). Если
// API_ID/API_HASH не заданы в окружении, просто логирует это и выходит —
// фича остаётся выключенной (Ready() == false навсегда), ничего не падает.
// botUsername (без @) — username основного бота (Bot API), нужен один раз,
// чтобы при первом запуске добавить его в релей-группу (см. relay.go).
func Start(ctx context.Context, botUsername string) {
	apiID, apiHash, ok := envConfig()
	if !ok {
		log.Printf("[userbot] API_ID/API_HASH не заданы — доставка FLAC через MTProto выключена")
		return
	}

	c := telegram.NewClient(apiID, apiHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: SessionPath()},
	})
	client = c

	go runForever(ctx, c, botUsername)
}

// runForever держит MTProto-соединение поднятым, переподключаясь при
// обрыве — по аналогии с тем, как основной бот (tgbot/bot.go, go b.Start())
// и torr.Manager работают вечно на весь жизненный цикл процесса.
func runForever(ctx context.Context, c *telegram.Client, botUsername string) {
	const reconnectDelay = 5 * time.Second
	for ctx.Err() == nil {
		err := c.Run(ctx, func(ctx context.Context) error {
			status, err := c.Auth().Status(ctx)
			if err != nil {
				return err
			}
			if !status.Authorized {
				log.Printf("[userbot] сессия не авторизована — выполните разовый логин: go run ./cmd/userbot-login")
				<-ctx.Done()
				return ctx.Err()
			}

			self, err := c.Self(ctx)
			if err != nil {
				return err
			}
			log.Printf("[userbot] подключено: @%s (id=%d)", self.Username, self.ID)

			if err := ensureRelayChannel(ctx, c.API(), botUsername); err != nil {
				// Не фатально для соединения — просто фича остаётся
				// недоступной (Ready() учитывает RelayChatID()), в логах
				// будет видно, что чинить.
				log.Printf("[userbot] релей-группа не готова: %v", err)
			}

			ready.Store(true)
			defer ready.Store(false)

			<-ctx.Done()
			return ctx.Err()
		})
		ready.Store(false)
		if ctx.Err() != nil {
			return
		}
		log.Printf("[userbot] соединение прервано: %v (переподключение через %v)", err, reconnectDelay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconnectDelay):
		}
	}
}

func envConfig() (apiID int, apiHash string, ok bool) {
	idStr := os.Getenv("API_ID")
	hash := os.Getenv("API_HASH")
	if idStr == "" || hash == "" {
		return 0, "", false
	}
	id, err := strconv.Atoi(idStr)
	if err != nil {
		log.Printf("[userbot] API_ID должен быть числом: %v", err)
		return 0, "", false
	}
	return id, hash, true
}
