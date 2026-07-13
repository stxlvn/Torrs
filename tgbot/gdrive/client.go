// Package gdrive зеркалирует скачанные файлы в Google Drive — резервная
// копия, независимая от Telegram (см. package-level комментарий на
// MirrorToDrive в tgbot/torr/manager.go). Использует OAuth2 личного
// Google-аккаунта (не service account — у личных Google-аккаунтов нет
// собственной квоты для service account без Workspace); авторизация —
// разовый интерактивный шаг через cmd/gdrive-login, дальше токен
// переиспользуется и обновляется автоматически (oauth2.Config.TokenSource).
package gdrive

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"

	"torrsru/global"
)

var (
	service *drive.Service
	ready   atomic.Bool
)

// Ready сообщает, настроен ли и авторизован ли клиент Google Drive прямо
// сейчас. Вызывающая сторона (MirrorToDrive в manager.go) должна проверять
// это перед вызовом — но UploadFile тоже проверяет сама, на всякий случай.
func Ready() bool {
	return ready.Load()
}

// TokenPath — путь к файлу с OAuth-токеном, тот же, что использует
// cmd/gdrive-login. GDRIVE_TOKEN переопределяет дефолт (рядом с
// бинарником — там же где userbot.session, см. .gitignore).
func TokenPath() string {
	if p := os.Getenv("GDRIVE_TOKEN"); p != "" {
		return p
	}
	return filepath.Join(global.PWD, "gdrive_token.json")
}

// OAuthConfig собирает oauth2.Config из окружения (GDRIVE_CLIENT_ID/
// GDRIVE_CLIENT_SECRET) — используется и здесь (Start), и в
// cmd/gdrive-login (сам логин), поэтому вынесено в общий пакет.
func OAuthConfig(redirectURL string) (*oauth2.Config, bool) {
	id := os.Getenv("GDRIVE_CLIENT_ID")
	secret := os.Getenv("GDRIVE_CLIENT_SECRET")
	if id == "" || secret == "" {
		return nil, false
	}
	return &oauth2.Config{
		ClientID:     id,
		ClientSecret: secret,
		Endpoint:     google.Endpoint,
		RedirectURL:  redirectURL,
		// drive.file — приложение видит только файлы/папки, которые само
		// создало (или которые пользователь явно открыл через него).
		// Полный доступ ко всему Drive не нужен для резервной заливки.
		Scopes: []string{drive.DriveFileScope},
	}, true
}

func loadToken(path string) (*oauth2.Token, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

// SaveToken сохраняет токен на диск — используется и здесь, и в
// cmd/gdrive-login после первичного обмена кода на токен.
func SaveToken(path string, tok *oauth2.Token) error {
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// Start поднимает клиент Google Drive (не блокирует, синхронная
// инициализация — в отличие от userbot, тут нет постоянного соединения,
// просто HTTP-клиент с автообновлением токена). Если
// GDRIVE_CLIENT_ID/GDRIVE_CLIENT_SECRET не заданы или токен ещё не
// авторизован (см. cmd/gdrive-login) — просто логирует это, Ready()
// остаётся false навсегда, ничего не падает.
func Start(ctx context.Context) {
	cfg, ok := OAuthConfig("")
	if !ok {
		log.Printf("[gdrive] GDRIVE_CLIENT_ID/GDRIVE_CLIENT_SECRET не заданы — резервное копирование на Google Drive выключено")
		return
	}
	tok, err := loadToken(TokenPath())
	if err != nil {
		log.Printf("[gdrive] сессия не авторизована (%v) — выполните разовый логин: go run ./cmd/gdrive-login", err)
		return
	}

	ts := cfg.TokenSource(ctx, tok)
	svc, err := drive.NewService(ctx, option.WithTokenSource(ts))
	if err != nil {
		log.Printf("[gdrive] не удалось создать клиент: %v", err)
		return
	}
	service = svc
	ready.Store(true)
	log.Printf("[gdrive] клиент готов")
}
