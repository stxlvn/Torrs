package torr

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var sacdSeen sync.Map

// isSACDFile — IsSACDImage с запоминанием: после извлечения треков образ
// удаляется, а конвейер спрашивает о нём ещё раз.
func isSACDFile(path string) bool {
	if _, ok := sacdSeen.Load(path); ok {
		return true
	}
	if IsSACDImage(path) {
		sacdSeen.Store(path, true)
		return true
	}
	return false
}

// IsSACDImage — ISO-образ Super Audio CD: Master TOC в секторе 510.
func IsSACDImage(path string) bool {
	if !strings.EqualFold(filepath.Ext(path), ".iso") {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 8)
	if _, err := f.ReadAt(buf, 510*2048); err != nil {
		return false
	}
	return string(buf) == "SACDMTOC"
}
