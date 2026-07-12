package main

import (
	"github.com/alexflint/go-arg"
	"log"
	"os"
	"path/filepath"
	"torrsru/db"
	"torrsru/global"
	"torrsru/tgbot"
	"torrsru/web"
)

func main() {
	var args struct {
		Port         string `default:"8094" arg:"-p" help:"port for http"`
		RebuildIndex bool   `default:"false" arg:"-r" help:"rebuild index and exit"`
		TMDBProxy    bool   `default:"false" arg:"--tmdb" help:"proxy for TMDB"`
		TGBotToken   string `arg:"--token" help:"telegram bot token"`
		TGHost       string `default:"http://127.0.0.1:8082" arg:"--tgapi" help:"local telegram api host"`
		TSHost       string `default:"http://127.0.0.1:8090" arg:"--ts" help:"TorrServer host"`
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

	global.TMDBProxy = args.TMDBProxy
	global.TSHost = args.TSHost

	db.Init()

	if args.RebuildIndex {
		if err := db.RebuildIndex(); err != nil {
			log.Println("Rebuild index error:", err)
		}
		return
	}

	if args.TGBotToken != "" {
		log.Printf("Запуск бота через локальный API: %s\n", args.TGHost)
		if err := tgbot.Start(args.TGBotToken, args.TGHost); err != nil {
			log.Println("Ошибка старта Telegram бота:", err)
		}
	}

	go web.Start(args.Port)

	// Блокируем выход, чтобы горутины бота и веба работали вечно
	select {}
}
