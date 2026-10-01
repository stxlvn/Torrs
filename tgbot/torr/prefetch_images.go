package torr

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dustin/go-humanize"

	"torrsru/tgbot/torr/state"
)

// prefetchFolderImages докачивает картинки КАЖДОЙ папки, где есть хотя бы
// один аудиофайл, который дойдёт до AudioProcessor'а (обычный трек или
// цельноальбомный cue-кандидат) — заранее, до того как runAllFiles успеет
// дойти в порядке торрента до самого аудиофайла и открыть по нему меню
// выбора обложки.
//
// Раньше (до отказа от отдельной "картинки первыми" фазы конвейера, см.
// runAllFiles) эта гарантия была побочным эффектом того, что ВСЕ картинки
// любой задачи полностью докачивались и выгружались первыми. Отказ от той
// фазы был осознанным решением (мелкие файлы вроде логов должны идти в
// чат в порядке торрента, а не вперемешку после всех картинок) — но
// обнажил реальный краевой случай: если обложка в самом торренте
// перечислена ПОСЛЕ аудиофайлов своей папки (например, "cover.jpg"
// последним файлом, а не первым — обычное дело, порядок в торренте не
// обязан совпадать с тем, что показывают файловые браузеры), findImagesInDir
// на стороне tgbot видел папку пустой, хотя обложка там объективно есть.
//
// Здесь та же цель (обложка на диске раньше, чем откроется меню её
// выбора), но точечно — только для папок, которым она реально нужна, и без
// переупорядочивания самой ДОСТАВКИ В ЧАТ: картинка всё равно отправится
// пользователю в её собственный черёд по порядку торрента (см.
// downloadFileToDisk — если файл уже лежит на диске с нужным размером,
// повторно из TorrServer он не скачивается, только досчитывается прогресс).
func prefetchFolderImages(wrk *Worker) {
	dirsWithAudio := make(map[string]bool)
	for _, fi := range wrk.fileIndices {
		f := wrk.ti.FileStats[fi]
		if isAudioExt(f.Path) && isProcessableAudio(f.Path) {
			dir := filepath.Dir(strings.TrimPrefix(f.Path, "/"))
			dirsWithAudio[dir] = true
		}
	}
	if len(dirsWithAudio) == 0 {
		return
	}

	var imageFiles []*state.TorrentFileStat
	for _, f := range wrk.ti.FileStats {
		if !isImageExt(f.Path) {
			continue
		}
		dir := filepath.Dir(strings.TrimPrefix(f.Path, "/"))
		if dirsWithAudio[dir] || dirsWithAudio[filepath.Dir(dir)] {
			imageFiles = append(imageFiles, f)
		}
	}

	defer wrk.setPrefetchStage("")
	for _, imgFile := range imageFiles {
		if wrk.isCancelled.Load() {
			return
		}
		if err := fetchImageToTmp(wrk, imgFile); err != nil {
			log.Printf("[audio] не удалось заранее скачать картинку %q: %v", imgFile.Path, err)
		}
	}
}

// fetchImageToTmp скачивает картинку целиком потоково (обложки-сканы могут
// быть от единиц КБ до сотен МБ — в отличие от crошечных .cue,
// io.ReadAll+WriteFile здесь означал бы держать весь файл в памяти разом)
// и кладёт её на диск туда же, куда обычный конвейер положил бы её в свой
// черёд — так downloadFileToDisk увидит уже готовый файл и не будет качать
// его повторно.
func fetchImageToTmp(wrk *Worker, imgFile *state.TorrentFileStat) error {
	torrFile, err := NewTorrFile(wrk, imgFile)
	if err != nil {
		return err
	}
	defer torrFile.Close()

	relPath := strings.TrimPrefix(imgFile.Path, "/")
	fullPath := filepath.Join(wrk.tmpDir, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return err
	}

	out, err := os.Create(fullPath)
	if err != nil {
		return err
	}
	defer out.Close()

	pw := &prefetchProgress{wrk: wrk, name: filepath.Base(relPath), total: imgFile.Length, start: time.Now()}
	n, err := io.Copy(io.MultiWriter(out, pw), torrFile)
	if err != nil {
		os.Remove(fullPath)
		return err
	}
	log.Printf("[audio] заранее скачана картинка %q (%d байт) -> %s", imgFile.Path, n, fullPath)
	return nil
}

// prefetchProgress показывает в статусе ход предзагрузки большого файла —
// на медленной раздаче скан в сотню мегабайт качается минутами, и без
// этого задача выглядит зависшей.
type prefetchProgress struct {
	wrk   *Worker
	name  string
	total int64
	done  int64
	start time.Time
	last  time.Time
}

func (p *prefetchProgress) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if time.Since(p.last) >= 3*time.Second {
		p.last = time.Now()
		speed := float64(p.done) / time.Since(p.start).Seconds()
		p.wrk.setPrefetchStage(fmt.Sprintf("%s — %s / %s, %s/s, осталось %s", p.name,
			humanize.Bytes(uint64(p.done)), humanize.Bytes(uint64(p.total)), humanize.Bytes(uint64(speed)),
			formatETA(float64(p.total-p.done), speed)))
	}
	return len(b), nil
}
