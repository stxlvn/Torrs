package tgbot

import (
	"context"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	tele "gopkg.in/telebot.v4"
)

const sacdExtractTimeout = 90 * time.Minute

var pendingSACD sync.Map // user_hash_fileHash -> cueFileCtx

// processSACD: образ Super Audio CD раскладывается sacd_extract на DSF-треки
// (с названиями из TOC диска), которые дальше идут обычным аудио-путём.
// Образ больше лимита можно вместо этого отправить архивом.
func processSACD(c tele.Context, fc cueFileCtx) error {
	if _, err := exec.LookPath("sacd_extract"); err != nil {
		log.Printf("[sacd] %s: sacd_extract не установлен", fc.AudioPath)
		c.Send("⚠️ Это образ SACD, но на сервере нет sacd_extract — отправляю образ как есть.")
		return proceedAsIs(c, fc, nil)
	}
	if !fc.Oversized {
		return extractSACD(c, fc)
	}
	key := userKey(c, fc.Hash, fc.fileHash())
	pendingSACD.Store(key, fc)
	size := ""
	if st, err := os.Stat(fc.AudioPath); err == nil {
		size = fmt.Sprintf(" (%.1f ГБ)", float64(st.Size())/(1<<30))
	}
	markup := &tele.ReplyMarkup{}
	markup.Inline(
		markup.Row(markup.Data("🎼 Извлечь треки", "\fsacdx", fc.Hash, fc.fileHash())),
		markup.Row(markup.Data(skipLabel(true, false), "\fsacdarc", fc.Hash, fc.fileHash())),
	)
	text := fmt.Sprintf("💿 <b>%s</b>%s — образ SACD.\n\nИзвлечь треки? DSD будет переведён в PCM 24 бит без потери слышимого диапазона и придёт FLAC-файлами с названиями с диска.",
		html.EscapeString(filepath.Base(fc.AudioPath)), size)
	if _, err := c.Bot().Send(c.Recipient(), text, markup, tele.ModeHTML); err != nil {
		pendingSACD.Delete(key)
		return extractSACD(c, fc)
	}
	return nil
}

func handleSACDChoice(c tele.Context, hash, fileHash string, extract bool) error {
	val, ok := pendingSACD.LoadAndDelete(userKey(c, hash, fileHash))
	if !ok {
		return c.Respond(&tele.CallbackResponse{Text: "Данные устарели"})
	}
	c.Bot().Delete(c.Message())
	fc := val.(cueFileCtx)
	if !extract {
		c.Respond(&tele.CallbackResponse{Text: "Архивирую"})
		return proceedAsIs(c, fc, nil)
	}
	c.Respond(&tele.CallbackResponse{Text: "Извлекаю треки"})
	return extractSACD(c, fc)
}

func runSACDExtract(iso, outDir, area string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sacdExtractTimeout)
	defer cancel()
	t0 := time.Now()
	out, err := exec.CommandContext(ctx, "sacd_extract", area, "-s", "-c", "-i", iso, "-o", outDir, "-y", outDir).CombinedOutput()
	var tracks []string
	filepath.WalkDir(outDir, func(p string, d fs.DirEntry, werr error) error {
		if werr == nil && !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".dsf") {
			tracks = append(tracks, p)
		}
		return nil
	})
	sort.Slice(tracks, func(i, j int) bool { return naturalLess(tracks[i], tracks[j]) })
	tail := string(out)
	if len(tail) > 800 {
		tail = tail[len(tail)-800:]
	}
	log.Printf("[sacd] %s %s: %d треков за %v, err=%v, вывод: %s", filepath.Base(iso), area, len(tracks), time.Since(t0), err, tail)
	if len(tracks) == 0 && err == nil {
		err = fmt.Errorf("треки не найдены")
	}
	return tracks, err
}

func linkImages(fromDir, toDir string) {
	for _, img := range findImagesInDir(fromDir) {
		dst := filepath.Join(toDir, filepath.Base(img))
		if os.Link(img, dst) == nil {
			continue
		}
		src, err := os.Open(img)
		if err != nil {
			continue
		}
		if out, err := os.Create(dst); err == nil {
			io.Copy(out, src)
			out.Close()
		}
		src.Close()
	}
}

func extractSACD(c tele.Context, fc cueFileCtx) error {
	UpdateAudioProgress(fc.RootTmp, fmt.Sprintf("💿 Извлечение треков из SACD: %s…", html.EscapeString(filepath.Base(fc.AudioPath))))
	defer UpdateAudioProgress(fc.RootTmp, "")
	outDir := filepath.Join(fc.dir(), ".sacd_"+fc.fileHash())
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		completeAudioTask(fc.RootTmp)
		return err
	}
	tracks, err := runSACDExtract(fc.AudioPath, outDir, "-2")
	if len(tracks) == 0 {
		os.RemoveAll(outDir)
		os.MkdirAll(outDir, 0o755)
		tracks, err = runSACDExtract(fc.AudioPath, outDir, "-m")
	}
	if len(tracks) == 0 {
		os.RemoveAll(outDir)
		c.Send(fmt.Sprintf("❌ Не удалось извлечь треки из SACD: %v", err))
		if fc.Oversized && fc.Fallback != nil {
			return proceedAsIs(c, fc, nil)
		}
		completeAudioTask(fc.RootTmp)
		return fmt.Errorf("sacd_extract: %v", err)
	}
	os.Remove(fc.AudioPath)

	dirs := map[string]bool{}
	for _, t := range tracks {
		dirs[filepath.Dir(t)] = true
	}
	for d := range dirs {
		linkImages(fc.dir(), d)
	}

	for range tracks {
		AddAudioTask(fc.RootTmp, 0)
	}
	completeAudioTask(fc.RootTmp)

	var lastErr error
	for i, t := range tracks {
		key := fmt.Sprintf("%s#sacd%d", audioCacheKey(fc.Hash, fc.FileID), i+1)
		if err := processAudioFileWithKey(c, t, fc.Hash, fc.RootTmp, key, nil); err != nil {
			log.Printf("[sacd] %s: %v", t, err)
			lastErr = err
		}
	}
	return lastErr
}
