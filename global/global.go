package global

import "github.com/gin-gonic/gin"

var (
	Route     *gin.Engine
	Stopped   = false
	PWD       = ""
	TMDBProxy = false
	TSHost    = ""

	// TGFilesDir — путь на диске, где локальный Telegram Bot API сервер
	// хранит загруженные боту файлы (bind-mount его рабочего каталога).
	// Используется как запасной способ прочитать файл напрямую с диска,
	// если HTTP-эндпоинт /file/bot<token>/... локального сервера
	// недоступен. Пусто — запасной путь отключён.
	TGFilesDir = ""

	SendFromWeb func(initData, msg string) error
)
