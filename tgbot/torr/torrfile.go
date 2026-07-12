package torr

import (
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"torrsru/global"
	"torrsru/tgbot/torr/state"
)

var ERR_STOPPED = errors.New("stopped")

// streamClient переиспользуется для всех файлов торрента, чтобы соединения
// с TorrServer держались в connection pool (keep-alive) вместо переустановки
// TCP/TLS-хендшейка на каждый скачиваемый файл.
var streamClient = &http.Client{
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConnsPerHost:   10,
	},
}

type TorrFile struct {
	hash   string
	name   string
	wrk    *Worker
	offset int64
	size   int64
	id     int

	resp *http.Response
}

func NewTorrFile(wrk *Worker, tfile *state.TorrentFileStat) (*TorrFile, error) {
	tf := new(TorrFile)
	tf.hash = wrk.torrentHash
	tf.name = filepath.Base(tfile.Path)
	tf.wrk = wrk
	tf.size = tfile.Length

	link := global.TSHost + "/stream?link=" + url.QueryEscape(wrk.torrentHash) + "&index=" + strconv.Itoa(tfile.Id) + "&play"
	resp, err := streamClient.Get(link)
	if err != nil {
		log.Printf("[torrfile] NewTorrFile: %s (index=%d): FAILED: %v", tf.name, tfile.Id, err)
		return nil, err
	}
	log.Printf("[torrfile] NewTorrFile: %s (index=%d): поток открыт, status=%s size=%d", tf.name, tfile.Id, resp.Status, tf.size)
	tf.resp = resp
	return tf, nil
}

func (t *TorrFile) Read(p []byte) (n int, err error) {
	if t.wrk.isCancelled.Load() {
		return 0, ERR_STOPPED
	}
	n, err = t.resp.Body.Read(p)
	t.offset += int64(n)
	return
}

func (t *TorrFile) Loaded() int64 {
	return t.size - t.offset
}

func (t *TorrFile) Close() {
	if t.resp != nil && t.resp.Body != nil {
		t.resp.Body.Close()
		t.resp = nil
	}
}
