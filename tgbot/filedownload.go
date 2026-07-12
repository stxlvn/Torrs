package tgbot

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	tele "gopkg.in/telebot.v4"
	"torrsru/global"
)

// botToken и botAPIHost дублируют то, что уже хранится внутри *tele.Bot,
// потому что downloadTelegramFile вызывается в т.ч. из audio.go, где под
// рукой есть только tele.Context.Bot(), возвращающий интерфейс tele.API без
// доступа к полям URL/Token конкретной реализации *tele.Bot.
var (
	botToken   string
	botAPIHost string
)

// downloadTelegramFile скачивает файл, загруженный пользователем боту.
// Сначала пробует обычный HTTP-эндпоинт локального Bot API сервера
// (/file/bot<token>/<file_path>) — так это должно работать в норме. В этом
// окружении, однако, у локального сервера getFile отрабатывает корректно, а
// сам HTTP-эндпоинт отдаёт 404 даже для файлов, реально лежащих на диске
// (подтверждено вручную: getFile -> file_path, затем GET по нему -> 404, в
// т.ч. изнутри контейнера). Поэтому запасной путь — прочитать файл напрямую
// с диска по global.TGFilesDir, куда смонтирован рабочий каталог сервера.
func downloadTelegramFile(b tele.API, f *tele.File) (io.ReadCloser, error) {
	filePath, err := resolveFilePath(b, f.FileID)
	if err != nil {
		return nil, fmt.Errorf("getFile: %w", err)
	}

	url := botAPIHost + "/file/bot" + botToken + "/" + filePath
	resp, httpErr := http.Get(url)
	if httpErr == nil && resp.StatusCode == http.StatusOK {
		return resp.Body, nil
	}
	if resp != nil {
		resp.Body.Close()
	}
	log.Printf("[bot] downloadTelegramFile: HTTP /file/ недоступен (err=%v), пробуем локальный диск (%s)", httpErr, filePath)

	if global.TGFilesDir == "" {
		return nil, fmt.Errorf("скачивание не удалось: HTTP /file/ недоступен, локальный каталог (--tgfiles) не задан")
	}
	localPath := filepath.Join(global.TGFilesDir, botToken, filePath)
	file, err := os.Open(localPath)
	if err != nil {
		return nil, fmt.Errorf("не удалось скачать файл ни по HTTP, ни с локального диска (%s): %w", localPath, err)
	}
	log.Printf("[bot] downloadTelegramFile: файл прочитан напрямую с диска: %s", localPath)
	return file, nil
}

// resolveFilePath — переиспользование логики (*tele.Bot).FileByID через
// интерфейс tele.API (Raw), т.к. FileByID есть только на конкретном типе
// *tele.Bot, а не в самом интерфейсе.
func resolveFilePath(b tele.API, fileID string) (string, error) {
	data, err := b.Raw("getFile", map[string]string{"file_id": fileID})
	if err != nil {
		return "", err
	}
	var resp struct {
		Result tele.File
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	return resp.Result.FilePath, nil
}
