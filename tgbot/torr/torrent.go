package torr

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"torrsru/global"
	"torrsru/tgbot/torr/state"
)

type TorrentDetails struct {
	Title   string
	Size    string
	Date    time.Time
	Link    string
	Tracker string
	Peer    int
	Seed    int
	Magnet  string
}

// tiCacheTTL — окно, в течение которого повторные запросы GetTorrentInfo
// для одного и того же hash отдаются из кэша без похода в TorrServer.
// Прогресс-бары и так обновляются не чаще раз в 5 секунд (см.
// updateDownloadStatus), а при частых нажатиях кнопок в меню файлов один и
// тот же торрент запрашивался бы по несколько раз подряд синхронно.
const tiCacheTTL = 2 * time.Second

type tiCacheEntry struct {
	mu      sync.Mutex
	ti      *state.TorrentStatus
	err     error
	fetched time.Time
}

var tiCache sync.Map // hash string -> *tiCacheEntry

func GetTorrentInfo(hash string) (*state.TorrentStatus, error) {
	val, _ := tiCache.LoadOrStore(hash, &tiCacheEntry{})
	entry := val.(*tiCacheEntry)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if !entry.fetched.IsZero() && time.Since(entry.fetched) < tiCacheTTL {
		return entry.ti, entry.err
	}

	ti, err := fetchTorrentInfo(hash)
	entry.ti = ti
	entry.err = err
	entry.fetched = time.Now()
	return ti, err
}

func fetchTorrentInfo(hash string) (*state.TorrentStatus, error) {
	link := global.TSHost + "/stream?stat&link=" + url.QueryEscape(hash)
	start := time.Now()
	resp, err := http.Get(link)
	if err != nil {
		log.Printf("[torrent] fetchTorrentInfo(%s): FAILED after %v: %v", hash, time.Since(start), err)
		return nil, err
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[torrent] fetchTorrentInfo(%s): чтение тела FAILED: %v", hash, err)
		return nil, err
	}

	var ti *state.TorrentStatus

	err = json.Unmarshal(buf, &ti)
	if err != nil {
		log.Printf("[torrent] fetchTorrentInfo(%s): разбор JSON FAILED: %v, body=%s", hash, err, truncate(buf, 300))
	}
	return ti, err
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// DownloadTorrentFile скачивает конкретный файл из торрента (по индексу в FileStats) и сохраняет во временный файл.
// Возвращает путь к временному файлу. Вызывающая сторона обязана удалить файл после использования.
func DownloadTorrentFile(hash string, fileIndex int) (string, error) {
	ti, err := GetTorrentInfo(hash)
	if err != nil {
		log.Printf("[torrent] DownloadTorrentFile(%s, %d): GetTorrentInfo FAILED: %v", hash, fileIndex, err)
		return "", err
	}
	if fileIndex < 0 || fileIndex >= len(ti.FileStats) {
		log.Printf("[torrent] DownloadTorrentFile(%s, %d): неверный индекс (всего файлов %d)", hash, fileIndex, len(ti.FileStats))
		return "", errors.New("неверный индекс файла")
	}
	fileStat := ti.FileStats[fileIndex]

	// Создаём временный Worker только для загрузки одного файла
	wrk := &Worker{
		torrentHash: hash,
	}

	tf, err := NewTorrFile(wrk, fileStat)
	if err != nil {
		log.Printf("[torrent] DownloadTorrentFile(%s, %d): NewTorrFile FAILED: %v", hash, fileIndex, err)
		return "", err
	}
	defer tf.Close()

	tmpFile, err := os.CreateTemp("", "torrimg_*"+filepath.Ext(fileStat.Path))
	if err != nil {
		return "", err
	}
	defer tmpFile.Close()

	_, err = io.Copy(tmpFile, tf)
	if err != nil {
		log.Printf("[torrent] DownloadTorrentFile(%s, %d): копирование FAILED: %v", hash, fileIndex, err)
		os.Remove(tmpFile.Name())
		return "", err
	}
	log.Printf("[torrent] DownloadTorrentFile(%s, %d): OK -> %s", hash, fileIndex, tmpFile.Name())
	return tmpFile.Name(), nil
}
