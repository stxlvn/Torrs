# Torrs bot

Telegram-бот для скачивания торрентов: принимает magnet-ссылку, хеш или
`.torrent`-файл, качает содержимое через [TorrServer](https://github.com/YouROK/TorrServer)
и заливает выбранные файлы в чат.

Умеет:

- файловый браузер по содержимому торрента (папки, множественный выбор);
- для аудио — читает/проставляет теги (исполнитель/название), встроенную
  или выбранную из раздачи обложку;
- нарезку альбома на треки по cue-sheet (в т.ч. если один `.cue` описывает
  сразу несколько физических файлов — например, релиз "2×LP");
- доставку FLAC **без перекодирования**, в оригинальном качестве — Bot API
  для `sendAudio` официально принимает только `.mp3`/`.m4a`, поэтому для
  FLAC используется второй, отдельный Telegram-аккаунт ("юзербот", через
  MTProto), см. [«Юзербот для lossless FLAC»](#юзербот-для-lossless-flac-опционально);
- файлы больше 1.9 ГБ — автоматически режет на тома 7-Zip (лимит self-hosted
  Bot API сервера — 2 ГБ на файл);
- кэш уже отправленных файлов по Telegram `file_id` — повторный запрос того
  же торрента/трека не качает и не обрабатывает всё заново, а пересылает
  уже загруженное;
- резервную копию всех скачанных файлов на Google Drive (опционально), см.
  [«Google Drive — резервная копия»](#9-google-drive--резервная-копия-опционально).

---

## Компоненты

Бот — это оркестратор поверх нескольких независимых сервисов:

| Компонент | Зачем | Как разворачивается |
|---|---|---|
| `torrs_bot` (этот репозиторий) | сама логика бота | собирается из исходников (Go) |
| [TorrServer](https://github.com/YouROK/TorrServer) | скачивает/раздаёт содержимое торрента по HTTP | готовый бинарник с GitHub Releases |
| [telegram-bot-api](https://github.com/tdlib/telegram-bot-api) (self-hosted) | локальный Bot API сервер — без него Telegram режет загрузку файлов до 50 МБ, а с ним лимит до 2 ГБ | Docker-образ или сборка из исходников |
| Юзербот (опционально) | доставка FLAC без перекодирования через MTProto | тот же бинарник `torrs_bot`, второй Telegram-аккаунт |

---

## Требования

Инструкция рассчитана на чистый VPS с **Ubuntu/Debian**; для других
дистрибутивов замените менеджер пакетов.

```bash
sudo apt update
sudo apt install -y git curl ffmpeg p7zip-full
```

- **git** — клонировать репозиторий.
- **ffmpeg** (даёт и `ffprobe`) — конвертация/нарезка аудио, определение
  длительности треков. Обязателен.
- **p7zip-full** (даёт бинарник `7z`) — архивация файлов > 1.9 ГБ на тома.
  Обязателен.
- **Go 1.25+** — компилятор. Если `go version` показывает более старую
  версию или Go не установлен:
  ```bash
  curl -fsSL -o go.tar.gz https://go.dev/dl/go1.25.0.linux-amd64.tar.gz
  sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go.tar.gz
  echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc && source ~/.bashrc
  go version
  ```
  (модуль проекта указывает `go 1.25.0` — при наличии более новой версии
  `GOTOOLCHAIN=auto` сам подтянет нужную при первой сборке).
- **Docker** — для локального Bot API сервера (см. ниже). Можно обойтись
  без него, собрав `telegram-bot-api` из исходников самостоятельно, но
  Docker-путь короче:
  ```bash
  curl -fsSL https://get.docker.com | sudo sh
  ```

---

## 1. Telegram-приложение (api_id/api_hash)

Понадобится и локальному Bot API серверу, и (если будете разворачивать)
юзерботу:

1. Откройте <https://my.telegram.org/apps>, войдите под своим номером.
2. Создайте приложение — получите `api_id` (число) и `api_hash` (строка).

## 2. Токен бота

Через [@BotFather](https://t.me/BotFather) командой `/newbot` создайте бота
и сохраните выданный токен (`BOT_TOKEN`).

## 3. TorrServer

```bash
curl -fsSL -o /root/TorrServer-linux-amd64 \
  https://github.com/YouROK/TorrServer/releases/latest/download/TorrServer-linux-amd64
chmod +x /root/TorrServer-linux-amd64
```

Юнит systemd:

```ini
# /etc/systemd/system/torrserver.service
[Unit]
Description=TorrServer
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/root
ExecStart=/root/TorrServer-linux-amd64 --torrentaddr :6881 --port=8090
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now torrserver
```

## 4. Локальный Bot API сервер

Проще всего — готовый Docker-образ (порт слушает только `127.0.0.1`, наружу
не торчит):

```bash
sudo docker run -d \
  --name telegram-bot-api \
  --restart unless-stopped \
  -p 127.0.0.1:8082:8081 \
  -e TELEGRAM_API_ID=<ваш_api_id> \
  -e TELEGRAM_API_HASH=<ваш_api_hash> \
  -v /tmp/telegram-bot-files:/var/lib/telegram-bot-api \
  aiogram/telegram-bot-api
```

Том `/tmp/telegram-bot-files` должен совпадать с флагом `--tgfiles` бота
(см. ниже) — это путь, откуда бот читает файлы напрямую с диска в обход
HTTP, если `/file/`-эндпоинт сервера недоступен.

Альтернатива без Docker — собрать `telegram-bot-api` из исходников по
[официальной инструкции](https://tdlib.github.io/telegram-bot-api/build.html)
и запустить с `--api-id`/`--api-hash` и `--local` (файловый доступ).

## 5. Сборка бота

Далее считаем, что бот живёт в `/opt/torrs` (любой другой путь тоже
годится — просто используйте его везде ниже вместо `/opt/torrs`):

```bash
sudo git clone https://github.com/stxlvn/Torrs.git /opt/torrs
cd /opt/torrs
sudo go build -o torrs_bot ./cmd/main
```

## 6. Конфигурация

`.env` рядом с бинарником:

```bash
sudo tee /opt/torrs/.env >/dev/null <<'EOF'
BOT_TOKEN=<токен от @BotFather>
API_ID=<api_id с my.telegram.org>
API_HASH=<api_hash с my.telegram.org>
EOF
```

`API_ID`/`API_HASH` нужны, только если планируете юзербота для lossless
FLAC (шаг 8) — без них бот прекрасно работает, просто FLAC будет уходить
перекодированным в ALAC/M4A через обычный Bot API.

## 7. systemd-юнит бота

```ini
# /etc/systemd/system/torrs.service
[Unit]
Description=Torrs Telegram Bot Service
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/opt/torrs
EnvironmentFile=/opt/torrs/.env
ExecStart=/opt/torrs/torrs_bot
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now torrs
journalctl -u torrs -f   # проверить, что стартовал без ошибок
```

Полный список CLI-флагов (все опциональны, есть разумные дефолты):

```bash
./torrs_bot --help
```

| Флаг | По умолчанию | Смысл |
|---|---|---|
| `--token` | из `BOT_TOKEN` | токен бота (флаг приоритетнее переменной окружения) |
| `--tgapi` | `http://127.0.0.1:8082` | адрес локального Bot API сервера |
| `--tgfiles` | `/tmp/telegram-bot-files` | путь к файлам Bot API сервера (см. шаг 4) |
| `--ts` | `http://127.0.0.1:8090` | адрес TorrServer |

---

## 8. Юзербот для lossless FLAC (опционально)

Без этого шага всё работает — FLAC просто конвертируется в ALAC/M4A перед
отправкой (как того требует Bot API). Юзербот убирает эту конвертацию:
второй Telegram-аккаунт (может быть любым, в т.ч. личным) заливает
оригинальный FLAC в служебную приватную группу, которую создаёт сам при
первом запуске и добавляет туда бота администратором; бот копирует
сообщение оттуда пользователю.

**Ограничение Telegram, не связанное с кодом:** юзербот не может написать
пользователю в чат первым — можно только через посредника вроде этой
служебной группы, что и реализовано.

1. `API_ID`/`API_HASH` в `.env` уже должны быть заданы (шаг 6).
2. Разовый логин (номер телефона → код из Telegram/SMS → 2FA-пароль при
   необходимости). Нужен реальный интерактивный терминал:
   ```bash
   cd /opt/torrs
   PHONE=+79991234567 go run ./cmd/userbot-login
   ```
   Придёт код, повторите с ним:
   ```bash
   PHONE=+79991234567 CODE=12345 go run ./cmd/userbot-login
   # если аккаунт защищён 2FA:
   PHONE=+79991234567 CODE=12345 PASSWORD=<пароль> go run ./cmd/userbot-login
   ```
   Сессия сохранится в `userbot.session` рядом с бинарником (путь
   переопределяется переменной `USERBOT_SESSION`) — **не коммитьте и не
   публикуйте этот файл**, это полноценный доступ к аккаунту.
3. Перезапустите `torrs`:
   ```bash
   sudo systemctl restart torrs
   journalctl -u torrs -f | grep userbot
   ```
   Должно появиться `[userbot] подключено: ...` и `[userbot] релей-группа
   создана ...`.

---

## 9. Google Drive — резервная копия (опционально)

Без этого шага всё работает как обычно — просто без дублирующей копии на
Google Drive. Со включённым бэкапом каждый скачанный файл (все выбранные,
включая аудио) дополнительно, не блокируя и не задерживая доставку в
Telegram, заливается на Google Drive — в папку `TorrsBackup/<название
раздачи>/`, сырым файлом без архивации и перекодирования. Если заливка не
удалась (нет сети, кончилась квота Google и т.п.) — это только пишется в
лог, задача в Telegram не прерывается и не повторяется из-за этого.

1. Получите `GDRIVE_CLIENT_ID`/`GDRIVE_CLIENT_SECRET`:
   - зайдите в [Google Cloud Console](https://console.cloud.google.com/apis/credentials),
     создайте (или выберите) проект;
   - включите **Google Drive API** (APIs & Services → Library);
   - создайте учётные данные: **Create Credentials → OAuth client ID**, тип
     **Desktop app**;
   - сохраните `Client ID` и `Client secret`.
2. Добавьте их в `.env`:
   ```bash
   sudo tee -a /opt/torrs/.env >/dev/null <<'EOF'
   GDRIVE_CLIENT_ID=<client id>
   GDRIVE_CLIENT_SECRET=<client secret>
   EOF
   ```
3. Разовый логин, в два шага через переменные окружения (без SSH-туннелей —
   Google больше не разрешает вставлять код руками без redirect, но
   redirect на loopback-адрес `127.0.0.1` по-прежнему принимается для
   Desktop-клиентов без явной регистрации, даже если там ничего не слушает
   — код всё равно виден в адресной строке):
   ```bash
   cd /opt/torrs
   GDRIVE_CLIENT_ID=... GDRIVE_CLIENT_SECRET=... go run ./cmd/gdrive-login
   ```
   Откроется ссылка — перейдите по ней в браузере на своём компьютере,
   войдите под тем Google-аккаунтом, куда хотите заливать бэкапы, и
   разрешите доступ. Страница после этого не загрузится (браузер попробует
   перейти на несуществующий `127.0.0.1:<порт>` — это нормально), но в
   адресной строке будет виден параметр `code=...` — скопируйте его
   значение (до следующего `&`) и запустите повторно с ним:
   ```bash
   GDRIVE_CLIENT_ID=... GDRIVE_CLIENT_SECRET=... CODE=<код> go run ./cmd/gdrive-login
   ```
   Токен сохранится в `gdrive_token.json` рядом с бинарником — **не
   коммитьте и не публикуйте этот файл**, это полноценный доступ к вашему
   Google Drive.
4. Перезапустите `torrs`:
   ```bash
   sudo systemctl restart torrs
   journalctl -u torrs -f | grep gdrive
   ```
   Должно появиться `[gdrive] клиент готов`.

---

## Структура рантайм-данных

Всё лежит рядом с бинарником `torrs_bot` (`WorkingDirectory` в юните) и не
должно коммититься в git (уже в `.gitignore`):

- `torrents.db` — локальная база (кэш Telegram `file_id` и т.п.);
- `userbot.session` — сессия юзербота, если настроен (шаг 8);
- `gdrive_token.json` — OAuth-токен Google Drive, если настроен (шаг 9);
- `.env` — секреты (токен бота, `api_id`/`api_hash`, при необходимости
  `GDRIVE_CLIENT_ID`/`GDRIVE_CLIENT_SECRET`).
