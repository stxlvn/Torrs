package userbot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync/atomic"

	"github.com/gotd/td/tg"

	"torrsru/db"
)

// relayChannelID/relayAccessHash — MTProto id и access_hash приватной
// супергруппы-релея. 0 означает "ещё не готова". Хранятся отдельно (а не
// одним атомиком со структурой), потому что здесь достаточно простого
// happens-before через Store/Load — читаются только вместе через
// RelayChatID/relayPeer, которые оба поля видят согласованно только после
// того, как ensureRelayChannel успешно отработает целиком.
var (
	relayChannelID  atomic.Int64
	relayAccessHash atomic.Int64
)

// RelayChatID возвращает Bot-API chat_id релей-группы (готов к
// использованию в tele.StoredMessage{ChatID: ...}), либо 0, если релей ещё
// не поднят.
func RelayChatID() int64 {
	id := relayChannelID.Load()
	if id == 0 {
		return 0
	}
	return channelIDToBotChatID(id)
}

// channelIDToBotChatID переводит "чистый" MTProto channel_id в тот вид,
// который использует Bot API для супергрупп/каналов: -1000000000000 минус
// сам id (общепринятая формула, используется во всех клиентских
// библиотеках Telegram — Bot API просто не имеет отдельного пространства
// идентификаторов, это одна и та же сущность, представленная по-разному).
func channelIDToBotChatID(channelID int64) int64 {
	return -1000000000000 - channelID
}

func relayPeer() *tg.InputPeerChannel {
	return &tg.InputPeerChannel{ChannelID: relayChannelID.Load(), AccessHash: relayAccessHash.Load()}
}

// ensureRelayChannel создаёт (при первом успешном запуске) приватную
// супергруппу, либо, если она уже была создана раньше, загружает её
// id/access_hash из db. В обоих случаях затем убеждается, что botUsername в
// ней состоит и является админом (см. promoteBotAdmin) — это отдельный,
// всегда выполняемый шаг: без прав админа Bot API не отдаёт боту доступ к
// сообщениям супергруппы по id (copyMessage/forwardMessage падают с "message
// to copy not found", даже если бот формально состоит в группе как рядовой
// участник), поэтому недостаточно сделать это только при первом создании.
func ensureRelayChannel(ctx context.Context, api *tg.Client, botUsername string) error {
	channelID, accessHash, ok := db.GetUserbotRelayChannel()
	if !ok {
		if botUsername == "" {
			return fmt.Errorf("не задан username бота — релей-группу создать не могу")
		}
		created, err := createRelayChannel(ctx, api)
		if err != nil {
			return fmt.Errorf("создание группы: %w", err)
		}
		channelID, accessHash = created.ID, created.AccessHash
		db.SaveUserbotRelayChannel(channelID, accessHash)
		log.Printf("[userbot] релей-группа создана (id=%d)", channelID)
	}
	relayChannelID.Store(channelID)
	relayAccessHash.Store(accessHash)

	if botUsername == "" {
		return nil
	}
	if err := promoteBotAdmin(ctx, api, channelID, accessHash, botUsername); err != nil {
		return fmt.Errorf("добавление @%s в релей-группу: %w", botUsername, err)
	}
	return nil
}

func createRelayChannel(ctx context.Context, api *tg.Client) (*tg.Channel, error) {
	updates, err := api.ChannelsCreateChannel(ctx, &tg.ChannelsCreateChannelRequest{
		Megagroup: true,
		Title:     "torrs FLAC relay",
		About:     "Служебная группа: юзербот кладёт сюда оригинальные FLAC, бот копирует их отсюда в чат с пользователем. Создано автоматически, не удалять.",
	})
	if err != nil {
		return nil, err
	}
	return extractCreatedChannel(updates)
}

// promoteBotAdmin добавляет botUsername в группу (если ещё не состоит — код
// USER_ALREADY_PARTICIPANT от InviteToChannel игнорируется) и выдаёт права
// администратора. Идемпотентно: ChannelsEditAdmin с теми же правами на уже
// админа — не более чем no-op на сервере Telegram.
func promoteBotAdmin(ctx context.Context, api *tg.Client, channelID, channelAccessHash int64, botUsername string) error {
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: botUsername})
	if err != nil {
		return fmt.Errorf("поиск @%s: %w", botUsername, err)
	}
	botUser, err := findUserByPeer(resolved.Users, resolved.Peer)
	if err != nil {
		return fmt.Errorf("поиск @%s: %w", botUsername, err)
	}

	inputChannel := &tg.InputChannel{ChannelID: channelID, AccessHash: channelAccessHash}
	inputBotUser := &tg.InputUser{UserID: botUser.ID, AccessHash: botUser.AccessHash}

	if _, err := api.ChannelsInviteToChannel(ctx, &tg.ChannelsInviteToChannelRequest{
		Channel: inputChannel,
		Users:   []tg.InputUserClass{inputBotUser},
	}); err != nil && !isAlreadyParticipant(err) {
		return fmt.Errorf("приглашение: %w", err)
	}

	// Права на приватную служебную группу без реальных других участников не
	// критичны — выдаём щедрый набор, чтобы не ловить похожую проблему
	// повторно на смежных правах.
	if _, err := api.ChannelsEditAdmin(ctx, &tg.ChannelsEditAdminRequest{
		Channel: inputChannel,
		UserID:  inputBotUser,
		AdminRights: tg.ChatAdminRights{
			ChangeInfo:     true,
			PostMessages:   true,
			EditMessages:   true,
			DeleteMessages: true,
			InviteUsers:    true,
			PinMessages:    true,
			ManageCall:     true,
			Other:          true,
		},
		Rank: "relay",
	}); err != nil {
		return fmt.Errorf("назначение админом: %w", err)
	}
	return nil
}

func isAlreadyParticipant(err error) bool {
	return err != nil && strings.Contains(err.Error(), "USER_ALREADY_PARTICIPANT")
}

func extractCreatedChannel(u tg.UpdatesClass) (*tg.Channel, error) {
	updates, ok := u.(*tg.Updates)
	if !ok {
		return nil, fmt.Errorf("неожиданный тип ответа: %T", u)
	}
	for _, chat := range updates.Chats {
		if ch, ok := chat.(*tg.Channel); ok {
			return ch, nil
		}
	}
	return nil, fmt.Errorf("канал не найден в ответе на создание")
}

func findUserByPeer(users []tg.UserClass, peer tg.PeerClass) (*tg.User, error) {
	pu, ok := peer.(*tg.PeerUser)
	if !ok {
		return nil, fmt.Errorf("неожиданный тип peer: %T", peer)
	}
	for _, u := range users {
		if user, ok := u.(*tg.User); ok && user.ID == pu.UserID {
			return user, nil
		}
	}
	return nil, fmt.Errorf("пользователь id=%d не найден среди %d в ответе resolveUsername", pu.UserID, len(users))
}
