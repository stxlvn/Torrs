package gdrive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"google.golang.org/api/drive/v3"
)

const rootFolderName = "TorrsBackup"
const folderMime = "application/vnd.google-apps.folder"

// ensureFolderMu сериализует ensureFolder: сама она делает "найти, иначе
// создать" двумя отдельными запросами без транзакции — если два разных
// файла из РАЗНЫХ задач (например, два разных пользователя качают раздачи
// с одинаковым названием одновременно) одновременно не находят папку и оба
// идут её создавать, на Drive появятся два дубликата TorrsBackup/<title>, и
// часть файлов осядет не в той папке. MirrorToDrive вызывается только
// последовательно ВНУТРИ одной задачи (см. runPipeline в manager.go), но
// НЕ между разными задачами — мьютекс закрывает именно этот межзадачный
// случай. Сама операция редкая и быстрая (папка почти всегда уже
// существует после первого файла), так что полная сериализация не бьёт по
// производительности.
var ensureFolderMu sync.Mutex

// uploadSem ограничивает число ОДНОВРЕМЕННЫХ заливок в Drive — без этого
// вызывающая сторона (MirrorToDrive в tgbot/bot.go) плодила бы по горутине
// на каждый файл конвейера без предела. Не путать с полным решением: файл
// уже открыт (см. OpenFile) и unlink'нут на стороне Telegram-выгрузки к
// моменту постановки в очередь на Drive, так что бюджет диска этим не
// закрывается полностью — если Drive заливает медленнее, чем идёт
// Telegram-выгрузка, "открытых, но ещё не залитых" файлов всё равно может
// накопиться больше uploadCap. Но хотя бы САМИ загрузки (и соответствующие
// сетевые соединения) не растут без предела — тот же принцип, что
// pipelineConcurrency в tgbot/torr/manager.go для выгрузки в Telegram.
const uploadCap = 3

var uploadSem = make(chan struct{}, uploadCap)

// AcquireUploadSlot блокируется, пока не освободится один из uploadCap
// слотов, и возвращает функцию для его освобождения (вызвать через defer).
func AcquireUploadSlot() func() {
	uploadSem <- struct{}{}
	return func() { <-uploadSem }
}

// OpenFile открывает localPath для последующей асинхронной заливки через
// UploadOpenFile. Открывать нужно СИНХРОННО и сразу после скачивания, до
// того как вызывающая сторона (конвейер в manager.go) успеет удалить
// localPath после выгрузки в Telegram: на Linux уже открытый файловый
// дескриптор переживает os.Remove (unlink не трогает открытые inode) — а
// вот открыть заново уже удалённый файл нельзя. Раз файл уже открыт здесь,
// саму заливку можно смело уводить в фон, не блокируя конвейер.
func OpenFile(localPath string) (*os.File, error) {
	return os.Open(localPath)
}

// UploadOpenFile заливает уже открытый f (см. OpenFile) в
// TorrsBackup/<folderTitle>/ на Google Drive, создавая обе папки при
// необходимости, и закрывает f по завершении. Без архивации и без
// перекодирования — сырой файл как есть на диске (в отличие от того, что
// в это же время может уйти в Telegram — 7z-тома, конвертированный звук
// и т.п.).
func UploadOpenFile(ctx context.Context, f *os.File, folderTitle string) error {
	defer f.Close()
	if !Ready() {
		return fmt.Errorf("gdrive: клиент не готов")
	}

	root, err := ensureFolder(ctx, rootFolderName, "")
	if err != nil {
		return fmt.Errorf("папка %s: %w", rootFolderName, err)
	}
	sub, err := ensureFolder(ctx, sanitizeFolderName(folderTitle), root)
	if err != nil {
		return fmt.Errorf("папка %s: %w", folderTitle, err)
	}

	_, err = service.Files.Create(&drive.File{
		Name:    filepath.Base(f.Name()),
		Parents: []string{sub},
	}).Media(f).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("заливка: %w", err)
	}
	return nil
}

func ensureFolder(ctx context.Context, name, parent string) (string, error) {
	ensureFolderMu.Lock()
	defer ensureFolderMu.Unlock()

	q := fmt.Sprintf("mimeType = %q and name = %q and trashed = false", folderMime, name)
	if parent != "" {
		q += fmt.Sprintf(" and %q in parents", parent)
	} else {
		q += " and 'root' in parents"
	}

	res, err := service.Files.List().Q(q).Fields("files(id)").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("поиск: %w", err)
	}
	if len(res.Files) > 0 {
		return res.Files[0].Id, nil
	}

	folder := &drive.File{
		Name:     name,
		MimeType: folderMime,
	}
	if parent != "" {
		folder.Parents = []string{parent}
	}
	created, err := service.Files.Create(folder).Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("создание: %w", err)
	}
	return created.Id, nil
}

func sanitizeFolderName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "unknown"
	}
	return name
}
