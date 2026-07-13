// Команда userbot-login — разовый логин MTProto-юзербота (см.
// tgbot/userbot). Telegram не даёт авторизоваться через API без этого шага
// (номер телефона -> код из SMS -> опционально пароль 2FA).
//
// Не интерактивна (нужно запускать в среде без настоящего TTY) — работает в
// два отдельных шага через переменные окружения:
//
//	# Шаг 1 — отправить код (уходит в Telegram/SMS на указанный номер):
//	PHONE=+79991234567 go run ./cmd/userbot-login
//
//	# Шаг 2 — ввести полученный код (и PASSWORD, если запросит 2FA):
//	PHONE=+79991234567 CODE=12345 go run ./cmd/userbot-login
//	PHONE=+79991234567 CODE=12345 PASSWORD=... go run ./cmd/userbot-login
//
// API_ID/API_HASH берутся из окружения (обычно из /root/torrs_project/config/.env).
// Между шагом 1 и шагом 2 сохраняется временный файл с phone_code_hash рядом
// с файлом сессии — без него SignIn невозможен. Сессия сохраняется в тот же
// файл, который потом использует основной бот (см. userbot.SessionPath).
// Повторный запуск после успешного логина ничего не ломает — просто
// сообщит, что сессия уже авторизована.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"

	"torrsru/global"
	"torrsru/tgbot/userbot"
)

func codeHashPath(sessionPath string) string {
	return sessionPath + ".codehash"
}

func extractPhoneCodeHash(sentCode tg.AuthSentCodeClass) (string, error) {
	s, ok := sentCode.(*tg.AuthSentCode)
	if !ok {
		return "", fmt.Errorf("неожиданный тип ответа отправки кода: %T", sentCode)
	}
	return s.PhoneCodeHash, nil
}

func main() {
	global.PWD, _ = os.Getwd()

	apiIDStr := os.Getenv("API_ID")
	apiHash := os.Getenv("API_HASH")
	if apiIDStr == "" || apiHash == "" {
		log.Fatal("не заданы API_ID/API_HASH в окружении (см. /root/torrs_project/config/.env)")
	}
	apiID, err := strconv.Atoi(apiIDStr)
	if err != nil {
		log.Fatalf("API_ID должен быть числом: %v", err)
	}

	phone := os.Getenv("PHONE")
	if phone == "" {
		log.Fatal("не задан PHONE (номер телефона юзербота, с +)")
	}
	code := os.Getenv("CODE")
	password := os.Getenv("PASSWORD")

	sessionPath := userbot.SessionPath()
	hashPath := codeHashPath(sessionPath)
	fmt.Printf("Сессия будет сохранена в %s\n", sessionPath)

	client := telegram.NewClient(apiID, apiHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: sessionPath},
	})

	ctx := context.Background()
	err = client.Run(ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("auth status: %w", err)
		}
		if status.Authorized {
			fmt.Println("Уже авторизован — повторный логин не требуется.")
			os.Remove(hashPath)
			return nil
		}

		authClient := client.Auth()

		if code == "" {
			// Шаг 1: отправляем код, сохраняем phone_code_hash для шага 2.
			sentCode, err := authClient.SendCode(ctx, phone, auth.SendCodeOptions{})
			if err != nil {
				return fmt.Errorf("send code: %w", err)
			}
			hash, err := extractPhoneCodeHash(sentCode)
			if err != nil {
				return err
			}
			if err := os.WriteFile(hashPath, []byte(hash), 0600); err != nil {
				return fmt.Errorf("сохранение phone_code_hash: %w", err)
			}
			fmt.Println("Код отправлен. Когда получите его, запустите снова с CODE=<код>:")
			fmt.Printf("  PHONE=%s CODE=<код из Telegram/SMS> go run ./cmd/userbot-login\n", phone)
			return nil
		}

		// Шаг 2: завершаем вход по коду (+ пароль 2FA при необходимости).
		hashBytes, err := os.ReadFile(hashPath)
		if err != nil {
			return fmt.Errorf("не найден сохранённый phone_code_hash (%s) — сначала выполните шаг 1 без CODE: %w", hashPath, err)
		}

		_, signInErr := authClient.SignIn(ctx, phone, code, string(hashBytes))
		if errors.Is(signInErr, auth.ErrPasswordAuthNeeded) {
			if password == "" {
				return errors.New("у аккаунта включена 2FA — повторите запуск, добавив PASSWORD=<пароль>")
			}
			if _, err := authClient.Password(ctx, password); err != nil {
				return fmt.Errorf("2FA-пароль не подошёл: %w", err)
			}
		} else if signInErr != nil {
			return fmt.Errorf("sign in: %w", signInErr)
		}

		os.Remove(hashPath)
		fmt.Println("Готово — сессия сохранена. Можно (пере)запускать torrs.")
		return nil
	})
	if err != nil {
		log.Fatalf("логин не удался: %v", err)
	}
}
