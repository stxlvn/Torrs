package db

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"torrsru/global"
	"torrsru/models/fdb"
)

var (
	mu     sync.Mutex
	isSync bool
)

func StartSync() {
	// Если FDBHOST не задан, синхронизация не требуется
	if _, ok := os.LookupEnv("FDBHOST"); !ok {
		log.Println("FDBHOST not set – skipping DB sync")
		return
	}

	for !global.Stopped {
		syncDB()
		time.Sleep(time.Minute * 20)
	}
}

func syncDB() {
	mu.Lock()
	if isSync {
		mu.Unlock()
		return
	}
	isSync = true
	defer func() { isSync = false }()

	filetime := GetFileTime()

	mu.Unlock()
	start := time.Now()
	gcCount := 0
	host := os.Getenv("FDBHOST")
	for {
		ftstr := strconv.FormatInt(filetime, 10)
		t := time.Unix(ft2sec(filetime), 0)
		log.Println("Fetch:", t.Format("2006-01-02 15:04:05"))
		resp, err := http.Get("http://" + host + "/sync/fdb/torrents?time=" + ftstr)
		if err != nil {
			log.Println("Error connect to fdb:", err)
			log.Println("Waiting 10 minutes before retrying...")
			time.Sleep(time.Minute * 10)
			continue
		}

		// Проверяем, что ответ не пустой
		if resp.StatusCode != http.StatusOK || resp.ContentLength == 0 {
			log.Printf("FDB sync: unexpected response (status %d, length %d). Skipping.\n", resp.StatusCode, resp.ContentLength)
			resp.Body.Close()
			break
		}

		var js *fdb.FDBRequest
		err = json.NewDecoder(resp.Body).Decode(&js)
		resp.Body.Close()
		if err != nil {
			log.Printf("Error decode json: %v (body might be empty or malformed)\n", err)
			break
		}

		err = saveTorrents(js.Collections)
		if err != nil {
			log.Fatal("Error save torrents:", err)
			return
		}

		torrents := 0
		for _, col := range js.Collections {
			if col.Value.FileTime > filetime {
				filetime = col.Value.FileTime
			}
			torrents += len(col.Value.Torrents)
		}

		err = SetFileTime(filetime)
		if err != nil {
			log.Fatal("Error set ftime:", err)
			return
		}

		t = time.Unix(ft2sec(filetime), 0)
		log.Println("Save:", t.Format("2006-01-02 15:04:05"), ", Torrents:", torrents)

		if !js.Nextread {
			break
		}
		js = nil
		gcCount++
		if gcCount > 10 {
			runtime.GC()
			gcCount = 0
		}
	}

	fmt.Println("End sync", time.Since(start))
}

func getHash(magnet string) string {
	pos := strings.Index(magnet, "btih:")
	if pos == -1 {
		return ""
	}
	magnet = magnet[pos+5:]
	pos = strings.Index(magnet, "&")
	if pos == -1 {
		return strings.ToLower(magnet)
	}
	return strings.ToLower(magnet[:pos])
}

func ft2sec(ft int64) int64 {
	return ft/10000000 - 11644473600
}
