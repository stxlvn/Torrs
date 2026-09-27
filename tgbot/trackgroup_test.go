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

func TestTrackGroupingWaitsForEarlierProducer(t *testing.T) {
	var sent [][]string
	orig := trackGroupSender
	trackGroupSender = func(_ tele.Context, items []readyTrack) {
		var names []string
		for _, it := range items {
			names = append(names, it.Title)
		}
		sent = append(sent, names)
	}
	defer func() { trackGroupSender = orig }()

	root, group := "/tmp/torrdl_order", "LP"
	order := func(n int) string { return fmt.Sprintf("LP/%05d", n) }
	add := func(n int) {
		enqueueTrack(nil, root, group, readyTrack{Title: fmt.Sprintf("%02d", n), Order: order(n)})
	}
	for n := 1; n <= 5; n++ {
		add(n)
	}
	side2 := startTrackProducer(nil, root, group, order(6))
	side3 := startTrackProducer(nil, root, group, order(11))
	for n := 11; n <= 14; n++ {
		side3.advance(order(n))
		add(n)
	}
	side3.done()
	if len(sent) != 0 {
		t.Fatalf("треки 11+ ушли раньше 6–10: %v", sent)
	}
	for n := 6; n <= 10; n++ {
		side2.advance(order(n))
		add(n)
	}
	side2.done()
	if len(sent) != 1 || strings.Join(sent[0], ",") != "01,02,03,04,05,06,07,08,09,10" {
		t.Fatalf("первая группа: %v", sent)
	}
	FlushTracks(root)
	if len(sent) != 2 || strings.Join(sent[1], ",") != "11,12,13,14" {
		t.Fatalf("остаток: %v", sent)
	}
}
