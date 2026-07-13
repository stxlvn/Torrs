package db

import (
	"strconv"

	bolt "go.etcd.io/bbolt"
)

func SaveTGFileID(fileID, tgID string) {
	db.Update(func(tx *bolt.Tx) error {
		ids, err := tx.CreateBucketIfNotExists([]byte("TGBotFileIDs"))
		if err != nil {
			return err
		}

		return ids.Put([]byte(fileID), []byte(tgID))
	})
}

func GetTGFileID(id string) string {
	ret := ""
	db.View(func(tx *bolt.Tx) error {
		ids := tx.Bucket([]byte("TGBotFileIDs"))
		if ids != nil {
			if b := ids.Get([]byte(id)); b != nil {
				ret = string(b)
			}
		}
		return nil
	})
	return ret
}

// SaveUserbotRelayChannel запоминает приватную супергруппу-релей, которую
// юзербот (tgbot/userbot) создаёt один раз при первом запуске: юзербот
// кладёт туда оригинальный FLAC, а бот (Bot API) копирует сообщение оттуда
// в реальный чат с пользователем. Нужен и id, и access_hash — оба требуются
// MTProto для любого обращения к каналу (InputChannel/InputPeerChannel).
func SaveUserbotRelayChannel(id, accessHash int64) {
	db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("UserbotRelay"))
		if err != nil {
			return err
		}
		if err := b.Put([]byte("id"), []byte(strconv.FormatInt(id, 10))); err != nil {
			return err
		}
		return b.Put([]byte("access_hash"), []byte(strconv.FormatInt(accessHash, 10)))
	})
}

// GetUserbotRelayChannel возвращает id и access_hash ранее созданной
// релей-группы, если она уже есть.
func GetUserbotRelayChannel() (id, accessHash int64, ok bool) {
	db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("UserbotRelay"))
		if b == nil {
			return nil
		}
		idBytes := b.Get([]byte("id"))
		hashBytes := b.Get([]byte("access_hash"))
		if idBytes == nil || hashBytes == nil {
			return nil
		}
		idN, err := strconv.ParseInt(string(idBytes), 10, 64)
		if err != nil {
			return nil
		}
		hashN, err := strconv.ParseInt(string(hashBytes), 10, 64)
		if err != nil {
			return nil
		}
		id, accessHash, ok = idN, hashN, true
		return nil
	})
	return id, accessHash, ok
}
