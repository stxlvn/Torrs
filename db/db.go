package db

import (
	bolt "go.etcd.io/bbolt"
	"log"
	"path/filepath"
	"time"
	"torrsru/global"
)

var db *bolt.DB

func Init() {
	d, err := bolt.Open(filepath.Join(global.PWD, "torrents.db"), 0o666, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		log.Fatalln("Error open db", err)
		return
	}
	db = d
}
