package torr

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

func GetTorrentInfo(hash string) (*state.TorrentStatus, error) {
	link := global.TSHost + "/stream?stat&link=" + url.QueryEscape(hash)
	resp, err := http.Get(link)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var ti *state.TorrentStatus

	err = json.Unmarshal(buf, &ti)
	return ti, err
}

// DownloadTorrentFile скачивает конкретный файл из торрента (по индексу в FileStats) и сохраняет во временный файл.
// Возвращает путь к временному файлу. Вызывающая сторона обязана удалить файл после использования.
func DownloadTorrentFile(hash string, fileIndex int) (string, error) {
	ti, err := GetTorrentInfo(hash)
	if err != nil {
		return "", err
	}
	if fileIndex < 0 || fileIndex >= len(ti.FileStats) {
		return "", errors.New("неверный индекс файла")
	}
	fileStat := ti.FileStats[fileIndex]

	// Создаём временный Worker только для загрузки одного файла
	wrk := &Worker{
		torrentHash: hash,
		isCancelled: false,
	}

	tf, err := NewTorrFile(wrk, fileStat)
	if err != nil {
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
		os.Remove(tmpFile.Name())
		return "", err
	}
	return tmpFile.Name(), nil
}
