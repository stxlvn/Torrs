// Команда gdrive-login — разовая авторизация резервного копирования на
// Google Drive (см. tgbot/gdrive). Google в 2022 отключил OOB-флоу
// (вставить код руками без редиректа) для новых OAuth-клиентов — но
// redirect_uri на loopback-адрес (127.0.0.1) по-прежнему разрешён для
// клиентов типа "Desktop app" без явной регистрации — но именно как IP-
// литерал, ИМЕННО "127.0.0.1", а не hostname "localhost" (иначе Google
// вернёт redirect_uri_mismatch). Порт на компьютере ничего не слушает —
// браузер после подтверждения попытается перейти на http://127.0.0.1:<port>/...
// и не сможет загрузить страницу, но код авторизации всё равно будет виден
// в адресной строке.
//
// Два шага через переменные окружения (без SSH-туннелей и локального
// сервера — просто откройте ссылку в обычном браузере на своём компьютере):
//
//	# Шаг 1 — получить ссылку:
//	GDRIVE_CLIENT_ID=... GDRIVE_CLIENT_SECRET=... go run ./cmd/gdrive-login
//
//	# Откройте ссылку в браузере, войдите в нужный Google-аккаунт,
//	# разрешите доступ. Страница НЕ загрузится (localhost недоступен на
//	# вашем компьютере) — это ожидаемо. Скопируйте параметр code из
//	# адресной строки (всё между "code=" и следующим "&").
//
//	# Шаг 2 — обменять код на токен:
//	GDRIVE_CLIENT_ID=... GDRIVE_CLIENT_SECRET=... CODE=<код> go run ./cmd/gdrive-login
//
// Токен сохранится в gdrive_token.json рядом с бинарником (переопределяется
// GDRIVE_TOKEN) — не коммитьте и не публикуйте этот файл, это полноценный
// доступ к вашему Google Drive.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"torrsru/global"
	"torrsru/tgbot/gdrive"
)

const port = "8721"

func main() {
	global.PWD, _ = os.Getwd()

	if tok, err := os.ReadFile(gdrive.TokenPath()); err == nil && len(tok) > 0 {
		fmt.Printf("Уже авторизован (%s существует) — повторный логин не требуется.\n", gdrive.TokenPath())
		return
	}

	redirectURL := "http://127.0.0.1:" + port
	cfg, ok := gdrive.OAuthConfig(redirectURL)
	if !ok {
		log.Fatal("не заданы GDRIVE_CLIENT_ID/GDRIVE_CLIENT_SECRET в окружении (создаются в Google Cloud Console: OAuth Client ID, тип Desktop app)")
	}

	code := os.Getenv("CODE")
	if code == "" {
		link := cfg.AuthCodeURL("state")
		fmt.Println("1. Откройте эту ссылку в браузере на СВОЁМ компьютере и войдите в тот")
		fmt.Println("   Google-аккаунт, куда хотите заливать бэкапы:")
		fmt.Println()
		fmt.Println("   " + link)
		fmt.Println()
		fmt.Println("2. После разрешения доступа страница НЕ загрузится (браузер попробует")
		fmt.Printf("   перейти на 127.0.0.1:%s — это нормально, там ничего не слушает).\n", port)
		fmt.Println("   Скопируйте значение параметра code из адресной строки (всё между")
		fmt.Println("   \"code=\" и следующим \"&\") и запустите:")
		fmt.Println()
		fmt.Printf("   GDRIVE_CLIENT_ID=%s GDRIVE_CLIENT_SECRET=<secret> CODE=<код> go run ./cmd/gdrive-login\n", os.Getenv("GDRIVE_CLIENT_ID"))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tok, err := cfg.Exchange(ctx, code)
	if err != nil {
		log.Fatalf("обмен кода на токен не удался: %v", err)
	}

	if err := gdrive.SaveToken(gdrive.TokenPath(), tok); err != nil {
		log.Fatalf("не удалось сохранить токен: %v", err)
	}

	fmt.Printf("Готово — токен сохранён в %s. Можно (пере)запускать torrs.\n", gdrive.TokenPath())
}
