package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/nchern/tgirc/pkg/irc"
	"github.com/nchern/tgirc/pkg/tg"
	"github.com/zelenin/go-tdlib/client"
	ircproto "gopkg.in/irc.v3"
)

const (
	serverName = "localhost"

	defaultIRCPort = 6667

	systemNick = "SysServ"
)

var (
	cfg config = config{
		appCacheDir: "./artifacts",
		apiIDRaw:    os.Getenv("API_ID"),
		apiHash:     os.Getenv("API_HASH"),
		phoneNumber: os.Getenv("PHONE_NUMBER"),
	}
)

type config struct {
	apiIDRaw    string
	apiHash     string
	appCacheDir string

	phoneNumber string
}

func (c *config) apiID() int32 {
	v, err := strconv.ParseInt(c.apiIDRaw, 10, 32)
	dieIf(err)
	return int32(v)
}

type Event struct {
	newSession *irc.Session
	ircUpdate  string
	tgUpdate   client.Type
}

func slugify(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, " ", "_"),
		"\t", "_")
}

func init() {
	base := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		AddSource: true,
		Level:     slog.LevelDebug,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if isTimestampDisabled() && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	slog.SetDefault(slog.New(errorTypeHandler{next: base}))
}

type errorTypeHandler struct {
	next slog.Handler
}

func (h errorTypeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h errorTypeHandler) Handle(ctx context.Context, r slog.Record) error {
	rr := r.Clone()
	r.Attrs(func(a slog.Attr) bool {
		if err, ok := a.Value.Any().(error); ok {
			rr.AddAttrs(slog.String(a.Key+"_type", fmt.Sprintf("%T", err)))
		}
		return true
	})
	return h.next.Handle(ctx, rr)
}

func (h errorTypeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return errorTypeHandler{next: h.next.WithAttrs(attrs)}
}

func (h errorTypeHandler) WithGroup(name string) slog.Handler {
	return errorTypeHandler{next: h.next.WithGroup(name)}
}

func isTimestampDisabled() bool { return os.Getenv("LOG_DISABLE_TIMESTAMP") != "" }

func startTg(events chan<- *Event) (*client.Client, *client.User, error) {
	params := &client.SetTdlibParametersRequest{
		UseTestDc: false,

		FilesDirectory:    filepath.Join(cfg.appCacheDir, ".tdlib", "files"),
		DatabaseDirectory: filepath.Join(cfg.appCacheDir, ".tdlib", "database"),

		UseChatInfoDatabase: true,
		UseFileDatabase:     true,
		UseMessageDatabase:  true,
		UseSecretChats:      false,

		ApiId:   cfg.apiID(),
		ApiHash: cfg.apiHash,

		SystemLanguageCode: "en",
		DeviceModel:        "Server",
		// SystemVersion:          "1.0.0",
		ApplicationVersion: "0.0.1",
		// IgnoreFileNames:    false,

		// Seems this setting prompts to clear authentication
		// EnableStorageOptimizer: true,
	}

	authorizer := client.ClientAuthorizer(params)
	go client.CliInteractor(authorizer)

	authorizer.PhoneNumber <- cfg.phoneNumber
	if _, err := client.SetLogVerbosityLevel(&client.SetLogVerbosityLevelRequest{
		NewVerbosityLevel: 1,
	}); err != nil {
		return nil, nil, err
	}

	tg, err := client.NewClient(authorizer)
	if err != nil {
		return nil, nil, err
	}

	opt, err := client.GetOption(&client.GetOptionRequest{
		Name: "version",
	})
	if err != nil {
		return nil, nil, err
	}
	slog.Info("TDLib version:", "version", opt.(*client.OptionValueString).Value)

	me, err := tg.GetMe()
	if err != nil {
		return nil, nil, err
	}
	return tg, me, nil
}

func processTgUpdates(events chan *Event, tg *client.Client) {
	listener := tg.GetListener()
	defer listener.Close()

	for update := range listener.Updates {
		events <- &Event{tgUpdate: update}
	}
}

func onUpdateChatLastMessage(state *State, up *client.UpdateChatLastMessage) error {
	chat, err := state.tg.GetChat(up.ChatId)
	if err != nil {
		return err
	}
	if err := onNewChat(state, chat); err != nil {
		return err
	}
	slog.Debug("onUpdateChatLastMessage:",
		"chat_id", up.ChatId,
		"last_message", chat.LastMessage != nil,
	)
	if chat.LastMessage != nil {
		state.sentToTg[chat.LastMessage.Id] = true
	}
	return nil
}

func fetchChatMembers(ts TGSession, chat *tg.Chat) ([]*tg.User, error) {
	var ids []int64
	switch t := chat.Type.(type) {
	case *client.ChatTypePrivate:
		ids = []int64{t.UserId}
	case *client.ChatTypeBasicGroup:
		info, err := ts.GetBasicGroupFullInfo(
			&client.GetBasicGroupFullInfoRequest{BasicGroupId: t.BasicGroupId})
		if err != nil {
			return nil, err
		}
		ids = make([]int64, 0, len(info.Members))
		for _, m := range info.Members {
			if u, ok := m.MemberId.(*client.MessageSenderUser); ok {
				ids = append(ids, u.UserId)
			}
		}
	default:
		return nil, errors.New("fetchChatMembers: unsupported chat type: " + chat.Type.ChatTypeType())
	}
	res := make([]*tg.User, 0, len(ids))
	for _, id := range ids {
		u, err := ts.GetUser(id)
		if err != nil {
			return nil, err
		}
		res = append(res, u)
	}
	return res, nil
}

func onNewChat(state *State, newChat *tg.Chat) error {
	mbrs, err := fetchChatMembers(state.tg, newChat)
	if err != nil {
		return err
	}
	state.registerChat(newChat.SetMembers(mbrs...))
	return nil
}

func onDeleteMessages(state *State, up *client.UpdateDeleteMessages) error {
	if up.FromCache {
		// ignore deletions from cache
		// https://github.com/tdlib/td/issues/620#issuecomment-513900431
		return nil
	}
	chat, err := state.tg.GetChat(up.ChatId)
	if err != nil {
		return err
	}
	s := fmt.Sprintf("%d message(s) got deleted from chat [%s]", len(up.MessageIds), chat.Title)
	return state.irc.SendPrivMsg(systemNick, state.tg.User().IRCNickname(chat), s)
}

func onUpdate(state *State, update client.Type) error {
	slog.Debug("onUpdate:",
		"class", update.GetClass(),
		"type", update.GetType(),
	)

	switch up := update.(type) {
	case *client.UpdateUserStatus:
		slog.Info("onUpdate: new status:", "status", up.Status.UserStatusType())
	case *client.UpdateNewChat:
		if err := onNewChat(state, tg.NewChat(up.Chat)); err != nil {
			return fmt.Errorf("onUpdateNewChat : %w", err)
		}
	case *client.UpdateNewMessage:
		if err := onNewMessage(state, up.Message); err != nil {
			return fmt.Errorf("onNewMessage : %w", err)
		}
	case *client.UpdateDeleteMessages:
		if err := onDeleteMessages(state, up); err != nil {
			return fmt.Errorf("onDeleteMessages : %w", err)
		}
	case *client.UpdateChatLastMessage:
		if err := onUpdateChatLastMessage(state, up); err != nil {
			return fmt.Errorf("onUpdateChatLastMessage : %w", err)
		}
	case *client.UpdateMessageSendSucceeded:
		// For some reason SendMessage creates 2(!) messsages and the one
		// of them is declared "old" when Send ack arrives
		// register new message:
		state.sentToTg[up.Message.Id] = true
	}
	return nil
}

func onNewMessage(state *State, m *client.Message) error {
	if state.sentToTg[m.Id] {
		// no need to send a message that has been sent from IRC clients
		return nil
	}
	chat, err := state.tg.GetChat(m.ChatId)
	if err != nil {
		return err
	}
	if !chat.Supported() {
		return nil
	}
	msg := tg.NewMessage(m, chat.Chat)
	if msg.SenderID() > 0 {
		u, err := state.tg.GetUser(msg.SenderID())
		if err != nil {
			return fmt.Errorf("c.GetUser : %d %w", msg.SenderID(), err)
		}
		msg.Sender = u
	}
	slog.Debug("incoming msg:",
		"user_id", msg.SenderID(),
		"chat_title", msg.Chat.Title,
		"irc_nick", msg.Sender.IRCNickname(chat),
		"chat_id", msg.ChatId,
		"chat_type", chat.Type.ChatTypeType(),
		"message_id", msg.Id,
		"first_line", msg.FirstLine(),
	)

	sender := msg.Sender.IRCNickname(chat)

	// works model when we map each user to a channel in IRC
	// it mirrors nick given in irc in case the sender is myself
	// so users are consistently shown by IRC client:
	// <yourself>
	// <recepient> ~ channel
	if msg.Sender.Id == state.tg.User().Id {
		sender = state.irc.Nick
	}
	rcpt := chat.ChannelName()
	if !state.isJoined(chat) {
		// join explicitly if not already jonied
		if err := handleIRCJoinToChannels(state, chat.ChannelName()); err != nil {
			return err
		}
	}
	for _, ln := range strings.Split(msg.Text(), "\n") {
		if err := state.irc.SendPrivMsg(sender, rcpt, ln); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	slog.Info("starting tg",
		"app_id", cfg.apiIDRaw,
		"phone", cfg.phoneNumber,
		"app_cache_dir", cfg.appCacheDir,
	)

	events := make(chan *Event)
	tgClient, me, err := startTg(events)
	dieIf(err)

	slog.Info("Me:",
		"first_name", me.FirstName,
		"last_name", me.LastName,
		"usernames", me.Usernames,
	)

	state := NewState(tg.NewSession(tgClient, tg.NewUser(me)))

	dieIf(populateChats(state))

	go processTgUpdates(events, tgClient)
	go mainEventLoop(state, events)

	dieIf(serveIRCAndWait(events))
}

func populateChats(state *State) error {
	chatIDs, err := state.tg.GetChats(&client.GetChatsRequest{Limit: 400})
	if err != nil {
		return err
	}
	for _, id := range chatIDs.ChatIds {
		chat, err := state.tg.GetChat(id)
		if err != nil {
			slog.Error("populateChats.GetChat:",
				"err", err,
			)
			continue
		}
		if !chat.Supported() {
			continue
		}
		mbrs, err := fetchChatMembers(state.tg, chat)
		if err != nil {
			return err
		}
		state.registerChat(chat.SetMembers(mbrs...))
	}
	// TODO: enable async loading
	// if _, err = tg.LoadChats(&client.LoadChatsRequest{Limit: 400}); err != nil {
	// 	return nil, err
	// }
	return nil
}

func serveIRCAndWait(events chan *Event) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", defaultIRCPort))
	if err != nil {
		return err
	}
	defer listener.Close()

	slog.Info("IRC server started on port 6667")
	for {
		conn, err := listener.Accept()
		if err != nil {
			slog.Error("Error accepting connection:",
				"err", err,
			)
			continue
		}
		slog.Info("irc incoming connection:", "remote_addr", conn.RemoteAddr())
		go handleIRCConnection(events, irc.NewSession(conn))
	}
}

type TGSession interface {
	GetBasicGroupFullInfo(*client.GetBasicGroupFullInfoRequest) (*client.BasicGroupFullInfo, error)

	GetChat(chatID int64) (*tg.Chat, error)
	GetChats(*client.GetChatsRequest) (*client.Chats, error)

	GetNetworkStatistics(*client.GetNetworkStatisticsRequest) (*client.NetworkStatistics, error)

	GetUser(userID int64) (*tg.User, error)

	Send(chatID int64, text string) (*client.Message, error)

	User() *tg.User

	ViewMessages(chatID int64, messageIDs ...int64) error
}

type State struct {
	tg TGSession

	irc *irc.Session

	// sentToTg collects all messages that were sent to Telegram
	// by this service. This can be used to track if we need to relay an
	// incoming message to IRC client
	sentToTg map[int64]bool

	chats       map[int64]*tg.Chat
	joinedChats map[string]*tg.Chat
}

func NewState(tgSession TGSession) *State {
	return &State{
		tg: tgSession,

		irc: irc.NewEmptySession(),

		sentToTg: map[int64]bool{},

		chats:       map[int64]*tg.Chat{},
		joinedChats: map[string]*tg.Chat{},
	}
}

func (s *State) isJoined(chat *tg.Chat) bool {
	return s.joinedChats[chat.ChannelName()] != nil
}

func (s *State) hasChat(channelName string) bool {
	// XXX: O(1) currently; fix it
	for _, v := range s.chats {
		if v.ChannelName() == channelName {
			return true
		}
	}
	return false
}

func (s *State) sortedChats() []*tg.Chat {
	chats := slices.Collect(maps.Values(s.chats))
	sort.Slice(chats, func(i, j int) bool {
		// push "Saved messages" chat to the top
		if len(chats[j].Members()) == 1 && chats[j].Members()[0].Id == s.tg.User().Id {
			return false
		}
		if chats[i].LastMessage == nil {
			return false
		}
		if chats[j].LastMessage == nil {
			return true
		}
		return chats[i].LastMessage.Date > chats[j].LastMessage.Date
	})
	return chats
}

func (s *State) registerChat(chat *tg.Chat) {
	slog.Debug("registerChat",
		"chat_id", chat.Id,
		"chat_title", chat.Title,
		"chat_type", chat.Type.ChatTypeType(),
		"slugged_title", slugify(chat.Title),
	)
	if !chat.Supported() {
		slog.Debug("registerChat: skip unsupported",
			"chat_id", chat.Id,
			"chat_title", chat.Title,
		)
		return
	}
	if strings.HasSuffix(strings.ToLower(chat.Title), "chat") {
		slog.Debug("registerChat: skip groups",
			"chat_id", chat.Id,
			"chat_title", chat.Title,
		)
		return
	}
	if s.chats[chat.Id] != nil {
		slog.Debug("registerChat already-registered",
			"chat_id", chat.Id,
			"slugged_title", slugify(chat.Title),
		)
	}
	s.chats[chat.Id] = chat
}

func (s *State) joinChatByName(name string) *tg.Chat {
	if s.joinedChats[name] != nil {
		return s.joinedChats[name]
	}
	for _, ch := range s.chats {
		if ch.ChannelName() == name {
			s.joinedChats[name] = ch
			return ch
		}
	}
	return nil
}

func (s *State) sendToTG(chatID int64, text string) (*client.Message, error) {
	m, err := s.tg.Send(chatID, text)
	if err != nil {
		return nil, err
	}
	s.sentToTg[m.Id] = true
	return m, nil
}

func mainEventLoop(state *State, events chan *Event) {
	for ev := range events {
		if ev.tgUpdate != nil {
			if err := onUpdate(state, ev.tgUpdate); err != nil {
				slog.Error("onUpdate:",
					"err", err,
				)
			}
		} else if ev.ircUpdate != "" {
			if err := handleIRCCommand(state, ev.ircUpdate); err != nil {
				slog.Error("handleCommand:",
					"session", state.irc,
					"err", err,
				)
				// need to close session
				state.irc.Close()
			}
		} else if ev.newSession != nil {
			if state.irc != nil && !state.irc.Closed() {
				if err := state.irc.SendPrivMsg(
					systemNick, state.tg.User().IRCNickname(nil),
					"New inbound IRC connection. Disconnect this session"); err != nil {
					slog.Error(fmt.Sprintf("%s %s", state.irc, err),
						"err", err,
					)
				}
				state.irc.Close()
			}
			state.irc = ev.newSession
		}
	}
}

func handleIRCConnection(events chan *Event, sess *irc.Session) {
	defer sess.Close()

	events <- &Event{newSession: sess}
	for {
		rawMsg, err := sess.Read()
		if err != nil {
			if err == io.EOF {
				slog.Info("disconnected", "session", sess)
				return
			}
			slog.Error("read from connection:",
				"session", sess,
				"err", err,
			)
			return
		}
		events <- &Event{ircUpdate: rawMsg}
	}
}

func handleSystemReplies(state *State, cmd *ircproto.Message) error {
	if cmd.Trailing() == "stats" {
		stats, err := state.tg.GetNetworkStatistics(
			&client.GetNetworkStatisticsRequest{OnlyCurrent: true})
		if err != nil {
			return err
		}
		for _, e := range stats.Entries {
			reply := fmt.Sprintf("Unknown stats: %T", e)
			switch ns := e.(type) {
			case *client.NetworkStatisticsEntryFile:
				reply = fmt.Sprintf("network_type=%s; sent_bytes=%d; received_bytes=%d",
					ns.NetworkType.NetworkTypeType(), ns.SentBytes, ns.ReceivedBytes)
			}
			if err := state.irc.SendPrivMsg(
				systemNick, state.tg.User().IRCNickname(nil), reply); err != nil {
				return fmt.Errorf("handleSystemReplies : %w", err)
			}
		}
		return nil
	}
	if err := state.irc.SendPrivMsg(
		systemNick, state.tg.User().IRCNickname(nil), "System is up and running."); err != nil {
		return fmt.Errorf("handleSystemReplies : %w", err)
	}
	return nil
}

func handleIRCJoinToChannels(state *State, channels ...string) error {
	sess := state.irc
	for _, cn := range channels {
		cn = strings.TrimSpace(cn)
		chat := state.joinChatByName(cn)
		if chat == nil {
			if _, err := sess.Write(irc.FormatMessage(serverName, ircproto.ERR_NOSUCHCHANNEL,
				state.irc.Nick, cn, "No such channel")); err != nil {
				return fmt.Errorf("%w: write to connection:", err)
			}
			continue
		}
		replies := []string{
			irc.FormatMessage(sess.Nick, "JOIN", cn),
			irc.FormatMessage(serverName, ircproto.RPL_TOPIC, sess.Nick, cn, chat.Topic()),
		}
		for _, m := range chat.Members() {
			replies = append(replies,
				irc.FormatMessage(serverName, ircproto.RPL_NAMREPLY, sess.Nick, "=", cn, m.IRCNickname(chat)))
		}
		replies = append(replies,
			irc.FormatMessage(serverName, ircproto.RPL_NAMREPLY, sess.Nick, "=", cn, "@"+systemNick))
		replies = append(replies, irc.FormatMessage(serverName, ircproto.RPL_ENDOFNAMES, sess.Nick, cn, "End of /NAMES list."))
		if _, err := sess.Write(replies...); err != nil {
			return fmt.Errorf("%w: write to connection:", err)
		}
	}
	return nil
}

func handleIRCPrivMessage(state *State, cmd *ircproto.Message) error {
	rcpt := cmd.Param(0)
	sess := state.irc
	chat := state.joinedChats[rcpt]
	if chat == nil {
		if _, err := sess.Write(irc.FormatMessage(serverName, ircproto.ERR_NOSUCHCHANNEL,
			state.irc.Nick, rcpt, "No such recepient")); err != nil {
			return fmt.Errorf("%w: write to connection:", err)
		}
		return nil
	}
	ac, err := state.tg.GetChat(chat.Id)
	if err != nil {
		return err
	}
	if _, err := state.sendToTG(chat.Chat.Id, cmd.Trailing()); err != nil {
		return err
	}
	if ac.LastMessage != nil {
		// Mark messages until last message in the chat as viewed
		// WHY: IRC does not have notifications when a message gets viewed
		// hence an assumption: when a user sends message from IRC to telegram,
		// they should have seen all the messages before this one
		if err := state.tg.ViewMessages(chat.Id, ac.LastMessage.Id); err != nil {
			// should not terminate this request
			slog.Error("tg.ViewMessages",
				"err", err,
			)
		}
	}
	return nil
}

const maxAutojoinChatsCount = 30

func autojoinTopContacts(state *State) error {
	chats := state.sortedChats()
	if len(chats) > maxAutojoinChatsCount {
		chats = chats[:maxAutojoinChatsCount]
	}
	for _, ch := range chats {
		if err := handleIRCJoinToChannels(state, ch.ChannelName()); err != nil {
			return err
		}
	}
	return nil
}

func handleIRCCommand(state *State, msg string) error {
	sess := state.irc
	command, err := ircproto.ParseMessage(strings.TrimSpace(msg))
	if err != nil {
		return err
	}
	slog.Info("received:", "session", sess, "command", command)

	// Session init sequence:
	// C: USER username 0 * :Real Name
	// C: NICK mynickname
	// S: :irc.example.com 001 mynickname :Welcome to the IRC Network mynickname!username@host
	switch command.Command {
	case "NICK":
		sess.Nick = command.Param(0)
	case "USER":
		sess.Username = command.Param(0)
		rpl := irc.FormatMessage(serverName, ircproto.RPL_WELCOME, sess.Nick,
			fmt.Sprintf("Welcome to the Telegram to IRC bridge %s!%s@localhost", sess.Nick, sess.Username))
		if _, err := sess.Write(rpl); err != nil {
			return fmt.Errorf("%w: write to connection:", err)
		}
		if err := state.irc.SendPrivMsg(
			systemNick, state.tg.User().IRCNickname(nil), "Hello!"); err != nil {
			return fmt.Errorf("write to connection : %w", err)
		}
		// TODO: implement auto-join on connection
		if err := autojoinTopContacts(state); err != nil {
			return err
		}
	case "PING":
		if _, err := sess.Write(irc.FormatMessage("", "PONG", "pong")); err != nil {
			return fmt.Errorf("%w: write to connection:", err)
		}
	case "CAP":
		switch command.Param(0) {
		case "LS":
			if _, err := sess.Write(irc.FormatMessage(serverName, "CAP", "*", "LS", "")); err != nil {
				return fmt.Errorf("%w: write to connection:", err)
			}
		case "REQ":
			if _, err := sess.Write(irc.FormatMessage(serverName, "CAP", "*", "NAK")); err != nil {
				return fmt.Errorf("%w: write to connection:", err)
			}
		case "END":
			slog.Info(fmt.Sprintf("%s %s", sess, command))
		default:
			return handleUnknownIRCCommand(sess, command.Command)
		}
	case "LIST":
		term := command.Param(0)
		termRx, err := regexp.Compile(term)
		if err != nil {
			slog.Error("handleIRCCommand", "err", err)
			if _, err := sess.Write(irc.FormatMessage(serverName, ircproto.ERR_NEEDMOREPARAMS,
				sess.Nick, "LIST", "Invalid filter")); err != nil {
				return fmt.Errorf("%w: write to connection:", err)
			}
			return nil
		}
		// 	obsolete in rfc281: ":localhost 321 mynickname Channel :Users  Name",
		// 	":localhost 322 mynickname #general 42 :General discussion channel",
		// 	":localhost 322 mynickname #random 15 :Random topics and fun",
		// 	":localhost 322 mynickname #help 5 :Get help and support",
		// 	":localhost 323 mynickname :End of /LIST",
		chats := state.sortedChats()
		replies := make([]string, 0, len(chats))
		for _, cht := range chats {
			if termRx.MatchString(cht.NormalizedName()) {
				replies = append(replies,
					irc.FormatMessage(serverName, ircproto.RPL_LIST,
						state.irc.Nick, cht.ChannelName(), "0", cht.Topic()))
			}
		}
		replies = append(replies,
			irc.FormatMessage(serverName, ircproto.RPL_LISTEND, state.irc.Nick, "End of /LIST"))
		if _, err := sess.Write(replies...); err != nil {
			return fmt.Errorf("%w: write to connection:", err)
		}
	case "MODE":
		// IRC clients request channel modes to sync local state
		// IRC: 324 RPL_CHANNELMODEIS
		channelName := command.Param(0)
		// minimum correct reply
		rpl := irc.FormatMessage(serverName, ircproto.RPL_CHANNELMODEIS, "MODE", state.irc.Nick, channelName, "+")
		if !state.hasChat(channelName) {
			// handle not exists clause
			rpl = irc.FormatMessage(serverName, ircproto.ERR_NOSUCHCHANNEL, state.irc.Nick, channelName, "No such channel")
		}
		if _, err := sess.Write(rpl); err != nil {
			return fmt.Errorf("%w: write to connection:", err)
		}
	case "QUIT":
		slog.Info("disconnected", "session", sess)
	case "PART":
		channelName := command.Param(0)
		slog.Info("parted", "session", sess, "channel_name", channelName)
		// :alice PART #general
		if _, err := sess.Write(irc.FormatMessage(sess.Nick, "PART", channelName)); err != nil {
			return fmt.Errorf("%w: write to connection:", err)
		}
	case "JOIN":
		channels := strings.Split(command.Param(0), ",")
		if err := handleIRCJoinToChannels(state, channels...); err != nil {
			return fmt.Errorf("%w: handleIRCJoinToChannels:", err)
		}
	case "PRIVMSG":
		rcpt := command.Param(0)
		if rcpt == systemNick {
			return handleSystemReplies(state, command)
		}
		return handleIRCPrivMessage(state, command)
	default:
		slog.Info("Unknown command:", "session", sess, "command", command)
		return handleUnknownIRCCommand(sess, command.Command)
	}
	return nil
}

func handleUnknownIRCCommand(sess *irc.Session, command string) error {
	rpl := irc.FormatMessage(serverName, ircproto.ERR_UNKNOWNCOMMAND,
		sess.Nick, command, "Unknown or unsupported command")
	if _, err := sess.Write(rpl); err != nil {
		return fmt.Errorf("%w: write to connection:", err)
	}
	return nil
}

func dieIf(err error) {
	if err != nil {
		slog.Error("fatal:",
			"err", err,
		)
		os.Exit(1)
	}
}
