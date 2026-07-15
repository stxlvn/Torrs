package main

import (
	"github.com/alexflint/go-arg"
	"log"
	"os"
	"path/filepath"
	"torrsru/db"
	"torrsru/global"
	"torrsru/tgbot"
)

func main() {
	var args struct {
		TGBotToken string `arg:"--token" help:"telegram bot token"`
		TGHost     string `default:"http://127.0.0.1:8082" arg:"--tgapi" help:"local telegram api host"`
		TGFilesDir string `default:"/tmp/telegram-bot-files" arg:"--tgfiles" help:"local telegram-bot-api file storage dir (fallback when its HTTP /file/ download is unavailable)"`
		TSHost     string `default:"http://127.0.0.1:8090" arg:"--ts" help:"TorrServer host"`
	}
	arg.MustParse(&args)

	// Приоритет токена: флаг -> переменная окружения BOT_TOKEN
	if args.TGBotToken == "" {
		args.TGBotToken = os.Getenv("BOT_TOKEN")
	}

	pwd := filepath.Dir(os.Args[0])
	pwd, _ = filepath.Abs(pwd)
	log.Println("PWD:", pwd)
	global.PWD = pwd

	global.TSHost = args.TSHost
	global.TGFilesDir = args.TGFilesDir

	db.Init()

	if args.TGBotToken != "" {
		log.Printf("Запуск бота через локальный API: %s\n", args.TGHost)
		if err := tgbot.Start(args.TGBotToken, args.TGHost); err != nil {
			// Раньше ошибка тут просто логировалась, а процесс уходил в
			// select{} навсегда — если локальный telegram-bot-api ещё не
			// поднялся (например, гонка при загрузке сервера), бот молча
			// зависал без polling'а до ручного перезапуска. Падаем с
			// ошибкой, чтобы systemd (Restart=always, RestartSec=5) сам
			// повторял попытки, пока API не станет доступен.
			log.Fatalln("Ошибка старта Telegram бота:", err)
		}
	}

	// Блокируем выход, чтобы горутины бота работали вечно
	select {}
}
