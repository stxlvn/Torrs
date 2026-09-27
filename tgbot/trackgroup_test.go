package tgbot

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	tele "gopkg.in/telebot.v4"
)

func TestTrackGrouping(t *testing.T) {
	var sent [][]string
	orig := trackGroupSender
	trackGroupSender = func(_ tele.Context, items []readyTrack) {
		cp := append([]readyTrack(nil), items...)
		sendTrackGroupSort(cp)
		var names []string
		for _, it := range cp {
			names = append(names, it.Title)
		}
		sent = append(sent, names)
	}
	defer func() { trackGroupSender = orig }()

	root := "/tmp/torrdl_test"
	order := rand.Perm(23)
	for _, i := range order[:12] {
		enqueueTrack(nil, root, "A", readyTrack{Title: fmt.Sprintf("A%02d", i+1), Order: fmt.Sprintf("A/%05d", i+1)})
	}
	enqueueTrack(nil, root, "B", readyTrack{Title: "B01", Order: "B/00001"})
	for _, i := range order[12:] {
		enqueueTrack(nil, root, "A", readyTrack{Title: fmt.Sprintf("A%02d", i+1), Order: fmt.Sprintf("A/%05d", i+1)})
	}
	if len(sent) != 2 || len(sent[0]) != 10 || len(sent[1]) != 10 {
		t.Fatalf("до досылки должно уйти 2 группы по 10: %v", sent)
	}
	FlushTracks(root)
	if len(sent) != 4 || len(sent[2]) != 3 || strings.Join(sent[3], ",") != "B01" {
		t.Fatalf("досылка остатка по папкам: %v", sent)
	}
	for _, g := range sent {
		for i := 1; i < len(g); i++ {
			if g[i-1] > g[i] {
				t.Fatalf("треки внутри группы не по порядку: %v", g)
			}
		}
	}
	total := 0
	for _, g := range sent {
		total += len(g)
	}
	if total != 24 {
		t.Fatalf("потеряны треки: %d из 24", total)
	}
	FlushTracks(root)
	if len(sent) != 4 {
		t.Fatal("повторная досылка отправила лишнее")
	}
}
