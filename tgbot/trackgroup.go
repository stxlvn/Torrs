package tgbot

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tele "gopkg.in/telebot.v4"

	"torrsru/db"
	"torrsru/tgbot/torr"
	"torrsru/tgbot/userbot"
)

const trackGroupSize = 10

// readyTrack — трек, полностью готовый к отправке (сконвертирован, с
// тегами), ждущий своей группы из 10.
type readyTrack struct {
	Path      string
	FileID    string
	Title     string
	Performer string
	Duration  int
	Cover     []byte
	CacheKey  string
	Order     string
	cleanup   []string
}

type trackQueue struct {
	mu     sync.Mutex
	sendMu sync.Mutex
	c      tele.Context
	items  []readyTrack
}

var trackQueues sync.Map // rootTmp + "\x00" + папка -> *trackQueue

var trackGroupSender = sendTrackGroup

// enqueueTrack копит треки папки и отправляет их альбомами по 10; остаток
// уходит через FlushTracks, когда все треки задачи готовы.
func enqueueTrack(c tele.Context, rootTmp, group string, t readyTrack) {
	v, _ := trackQueues.LoadOrStore(rootTmp+"\x00"+group, &trackQueue{})
	q := v.(*trackQueue)
	q.mu.Lock()
	q.c = c
	q.items = append(q.items, t)
	var batch []readyTrack
	if len(q.items) >= trackGroupSize {
		batch = append([]readyTrack(nil), q.items[:trackGroupSize]...)
		q.items = q.items[trackGroupSize:]
	}
	q.mu.Unlock()
	if batch != nil {
		q.sendMu.Lock()
		trackGroupSender(c, batch)
		q.sendMu.Unlock()
	}
}

func FlushTracks(rootTmp string) {
	prefix := rootTmp + "\x00"
	var keys []string
	trackQueues.Range(func(k, _ any) bool {
		if strings.HasPrefix(k.(string), prefix) {
			keys = append(keys, k.(string))
		}
		return true
	})
	sort.Slice(keys, func(i, j int) bool { return naturalLess(keys[i], keys[j]) })
	for _, k := range keys {
		v, ok := trackQueues.LoadAndDelete(k)
		if !ok {
			continue
		}
		q := v.(*trackQueue)
		q.mu.Lock()
		items, c := q.items, q.c
		q.items = nil
		q.mu.Unlock()
		if len(items) > 0 {
			q.sendMu.Lock()
			trackGroupSender(c, items)
			q.sendMu.Unlock()
		}
	}
}

func sendTrackGroupSort(items []readyTrack) {
	sort.SliceStable(items, func(i, j int) bool { return naturalLess(items[i].Order, items[j].Order) })
}

func sendTrackGroup(c tele.Context, items []readyTrack) {
	sendTrackGroupSort(items)
	defer func() {
		for _, it := range items {
			for _, p := range it.cleanup {
				os.Remove(p)
			}
		}
	}()
	for start := 0; start < len(items); start += trackGroupSize {
		sendTrackChunk(c, items[start:min(start+trackGroupSize, len(items))])
	}
}

func sendTrackChunk(c tele.Context, items []readyTrack) {
	var cached, fresh []readyTrack
	hasFLAC := false
	for _, it := range items {
		if it.FileID != "" {
			cached = append(cached, it)
			continue
		}
		fresh = append(fresh, it)
		if strings.EqualFold(filepath.Ext(it.Path), ".flac") {
			hasFLAC = true
		}
	}
	if len(cached) > 0 {
		sendViaBotAPI(c, cached)
	}
	if len(fresh) == 0 {
		return
	}
	if hasFLAC && userbot.Ready() {
		err := sendViaRelay(c, fresh)
		if err == nil {
			return
		}
		log.Printf("[audio] группа из %d треков не ушла через userbot (%v), откат на Bot API (ALAC)", len(fresh), err)
		fresh = flacToALAC(fresh)
	}
	sendViaBotAPI(c, fresh)
}

func sendViaRelay(c tele.Context, items []readyTrack) error {
	ctx := context.Background()
	if len(items) == 1 {
		it := items[0]
		msgID, chatID, err := userbot.SendToRelay(ctx, it.Path, it.Title, it.Performer, it.Duration, it.Cover)
		if err != nil {
			return err
		}
		sent, err := c.Bot().Copy(c.Recipient(), tele.StoredMessage{MessageID: strconv.Itoa(msgID), ChatID: chatID})
		if err != nil {
			return err
		}
		if it.CacheKey != "" && sent != nil && sent.Audio != nil && sent.Audio.FileID != "" {
			db.SaveTGFileID(it.CacheKey, sent.Audio.FileID)
		}
		log.Printf("[audio] %s: отправлено через userbot+релей (FLAC без перекодирования)", it.Path)
		return nil
	}
	var audios []userbot.RelayAudio
	for _, it := range items {
		audios = append(audios, userbot.RelayAudio{Path: it.Path, MIME: audioMIME(it.Path), Title: it.Title, Performer: it.Performer, Duration: it.Duration, Thumb: it.Cover})
	}
	t0 := time.Now()
	ids, chatID, err := userbot.SendAlbumToRelay(ctx, audios)
	if err != nil {
		return err
	}
	var msgs []tele.Editable
	for _, id := range ids {
		msgs = append(msgs, tele.StoredMessage{MessageID: strconv.Itoa(id), ChatID: chatID})
	}
	if _, err := c.Bot().CopyMany(c.Recipient(), msgs); err != nil {
		return err
	}
	log.Printf("[audio] группа из %d треков отправлена через userbot+релей за %v (%s … %s)", len(items), time.Since(t0), filepath.Base(items[0].Path), filepath.Base(items[len(items)-1].Path))
	return nil
}

func flacToALAC(items []readyTrack) []readyTrack {
	out := make([]readyTrack, 0, len(items))
	for _, it := range items {
		if strings.EqualFold(filepath.Ext(it.Path), ".flac") {
			if m4a, err := convertToM4A(it.Path); err == nil {
				it.cleanup = append(it.cleanup, m4a)
				it.Path = m4a
				embedCover(it.Path, it.Performer, it.Title, it.Cover)
			}
		}
		out = append(out, it)
	}
	return out
}

// embedCover вшивает обложку в файл для Bot API: у аудио в альбоме нельзя
// передать отдельное превью, клиенты берут вшитое.
func embedCover(path, artist, title string, cover []byte) {
	if len(cover) == 0 {
		return
	}
	tmp, err := writeTempCoverFile(cover)
	if err != nil {
		return
	}
	defer os.Remove(tmp)
	if err := writeAudioTags(path, artist, title, tmp); err != nil {
		log.Printf("[audio] %s: не удалось вшить обложку: %v", path, err)
	}
}

func audioMIME(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".flac":
		return "audio/flac"
	case ".m4a", ".mp4":
		return "audio/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".ogg":
		return "audio/ogg"
	}
	return "application/octet-stream"
}

func sendViaBotAPI(c tele.Context, items []readyTrack) {
	if len(items) == 1 {
		it := items[0]
		var err error
		if it.FileID != "" {
			err = sendCachedAudio(c, it.FileID, it.Title, it.Performer)
		} else {
			err = sendAudio(c, it.Path, it.Performer, it.Title, it.Duration, it.Cover, it.CacheKey)
		}
		if err != nil {
			log.Printf("[audio] %s: не отправлен: %v", it.Title, err)
		}
		return
	}

	var sendErr error
	for attempt := 1; attempt <= maxAudioSendRetries; attempt++ {
		var album tele.Album
		var files []*os.File
		for _, it := range items {
			a := &tele.Audio{Title: it.Title, Performer: it.Performer, Duration: it.Duration, MIME: audioMIME(it.Path)}
			if it.FileID != "" {
				a.File = tele.File{FileID: it.FileID}
			} else {
				f, err := os.Open(it.Path)
				if err != nil {
					sendErr = err
					break
				}
				files = append(files, f)
				a.File = tele.FromReader(f)
				a.FileName = filepath.Base(it.Path)
			}
			album = append(album, a)
		}
		var msgs []tele.Message
		if len(album) == len(items) {
			t0 := time.Now()
			msgs, sendErr = c.Bot().SendAlbum(c.Recipient(), album)
			if sendErr == nil {
				log.Printf("[audio] группа из %d треков отправлена через Bot API за %v", len(items), time.Since(t0))
			}
		}
		for _, f := range files {
			f.Close()
		}
		if sendErr == nil {
			for i, m := range msgs {
				if i < len(items) && items[i].CacheKey != "" && m.Audio != nil && m.Audio.FileID != "" {
					db.SaveTGFileID(items[i].CacheKey, m.Audio.FileID)
				}
			}
			return
		}
		delay := torr.FloodRetryDelay(sendErr, 5*time.Second)
		log.Printf("[audio] группа из %d треков: попытка %d/%d не удалась: %v (ждём %v)", len(items), attempt, maxAudioSendRetries, sendErr, delay)
		if attempt < maxAudioSendRetries {
			time.Sleep(delay)
		}
	}
	log.Printf("[audio] группа не отправилась, шлю треки по одному")
	for _, it := range items {
		sendViaBotAPI(c, []readyTrack{it})
	}
}
