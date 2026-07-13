package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nchern/tgirc/pkg/logger"
	"github.com/nchern/tgirc/pkg/tg"
	"github.com/zelenin/go-tdlib/client"
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

var errAlreadyClosed = errors.New("connection closed")

type config struct {
	apiIDRaw    string
	apiHash     string
	appCacheDir string

	phoneNumber string
}

func (c *config) apiID() int32 {
	logger.Debug.Println(c.apiIDRaw)
	v, err := strconv.ParseInt(c.apiIDRaw, 10, 32)
	dieIf(err)
	return int32(v)
}

type Event struct {
	newSession *session
	ircUpdate  string
	tgUpdate   client.Type
}

func slugify(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, " ", "_"),
		"\t", "_")
}

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

	recvFn := func(update client.Type) {
		events <- &Event{tgUpdate: update}
	}
	tg, err := client.NewClient(authorizer, client.WithResultHandler(client.NewCallbackResultHandler(recvFn)))
	if err != nil {
		return nil, nil, err
	}

	opt, err := client.GetOption(&client.GetOptionRequest{
		Name: "version",
	})
	if err != nil {
		return nil, nil, err
	}
	logger.Info.Printf("TDLib version: %s", opt.(*client.OptionValueString).Value)

	me, err := tg.GetMe(context.Background())
	if err != nil {
		return nil, nil, err
	}
	return tg, me, nil
}

// func processTgUpdates(events chan *Event, tg *client.Client) {
// 	listener := tg.GetListener()
// 	defer listener.Close()

// 	for update := range listener.Updates {
// 		events <- &Event{tgUpdate: update}
// 	}
// }

func onUpdateChatLastMessage(state *State, up *client.UpdateChatLastMessage) error {
	chat, err := state.tg.GetChat(up.ChatId)
	if err != nil {
		return err
	}
	if err := onNewChat(state, chat); err != nil {
		return err
	}
	fmt.Printf("onUpdateChatLastMessage: chat_id=%d last_message=%v \n",
		up.ChatId, chat.LastMessage != nil)
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
		info, err := ts.GetBasicGroupFullInfo(context.Background(),
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
		return nil, errors.New("fetchChatMembers: unsupported chat type: " + chat.Type.ChatTypeConstructor())
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
	state.registerChat(newChat.SetMembers(mbrs))
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
	return state.curSession.SendPrivMsg(systemNick, state.tg.User().IRCNickname(), s)
}

func onUpdate(state *State, update client.Type) error {
	logger.Debug.Printf("onUpdate: %s %s", update.GetConstructor(), update.GetType())

	switch up := update.(type) {
	case *client.UpdateUserStatus:
		logger.Info.Printf("onUpdate: new status: %s", up.Status.UserStatusConstructor())
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
	fmt.Printf("incoming msg: user_id=%d chat_title=%s irc_nick=%s chat_id=%d chat_type=%s message_id=%d %s\n",
		msg.SenderID(), msg.Chat.Title, msg.Sender.IRCNickname(), msg.ChatId,
		chat.Type.ChatTypeConstructor(), msg.Id, msg.FirstLine())

	sender := msg.Sender.IRCNickname()
	if msg.Sender.Id == state.tg.User().Id {
		sender = state.curSession.Nick
	}
	channel := chat.ChannelName()
	for _, ln := range strings.Split(msg.Text(), "\n") {
		if err := state.curSession.SendPrivMsg(sender, channel, ln); err != nil {
			return err
		}
	}
	return nil
}

func main() {
	events := make(chan *Event)

	tgClient, me, err := startTg(events)
	dieIf(err)

	logger.Info.Printf("Me: %s %s [%v]", me.FirstName, me.LastName, me.Usernames)

	state := NewState(tg.NewSession(tgClient, tg.NewUser(me)))

	dieIf(populateChats(state))

	// go processTgUpdates(events, tgClient)
	go mainEventLoop(state, events)

	dieIf(serveIRCAndWait(events))
}

func populateChats(state *State) error {
	chatIDs, err := state.tg.GetChats(context.Background(), &client.GetChatsRequest{Limit: 400})
	if err != nil {
		return err
	}
	for _, id := range chatIDs.ChatIds {
		chat, err := state.tg.GetChat(id)
		if err != nil {
			logger.Error.Printf("populateChats.GetChat: %s", err)
			continue
		}
		if !chat.Supported() {
			continue
		}
		mbrs, err := fetchChatMembers(state.tg, chat)
		if err != nil {
			return err
		}
		state.registerChat(chat.SetMembers(mbrs))
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

	logger.Info.Printf("IRC server started on port 6667")
	for {
		conn, err := listener.Accept()
		if err != nil {
			logger.Error.Printf("Error accepting connection: %s", err)
			continue
		}
		logger.Info.Printf("irc incoming connection: %s", conn.RemoteAddr())
		go handleConnection(events, newSession(conn))
	}
}

type TGSession interface {
	GetBasicGroupFullInfo(context.Context, *client.GetBasicGroupFullInfoRequest) (*client.BasicGroupFullInfo, error)

	GetChat(chatID int64) (*tg.Chat, error)
	GetChats(context.Context, *client.GetChatsRequest) (*client.Chats, error)

	GetNetworkStatistics(context.Context, *client.GetNetworkStatisticsRequest) (*client.NetworkStatistics, error)

	GetUser(userID int64) (*tg.User, error)

	Send(chatID int64, text string) (*client.Message, error)

	User() *tg.User

	ViewMessages(chatID int64, messageIDs ...int64) error
}

type State struct {
	tg TGSession

	curSession *session

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

		curSession: &session{closed: true},

		sentToTg: map[int64]bool{},

		chats:       map[int64]*tg.Chat{},
		joinedChats: map[string]*tg.Chat{},
	}
}

func (s *State) registerChat(chat *tg.Chat) {
	logger.Debug.Printf("registerChat chat_id=%d chat_title=%s chat_type=%s slugged_title=%s",
		chat.Id, chat.Title, chat.Type.ChatTypeConstructor(), slugify(chat.Title))
	if !chat.Supported() {
		logger.Debug.Printf("registerChat: skip unsupported chat_id=%d chat_title=%s", chat.Id, chat.Title)
		return
	}
	if strings.HasSuffix(strings.ToLower(chat.Title), "chat") {
		logger.Debug.Printf("registerChat: skip groups chat_id=%d chat_title=%s", chat.Id, chat.Title)
		return
	}
	if s.chats[chat.Id] != nil {
		logger.Debug.Printf("registerChat chat_id=%d slugged_title=%s already-registered",
			chat.Id, slugify(chat.Title))
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
				logger.Error.Printf("onUpdate: %T %s", err, err)
			}
		} else if ev.ircUpdate != "" {
			if err := handleIRCCommand(state, ev.ircUpdate); err != nil {
				logger.Error.Printf("%s handleCommand: %T %s", state.curSession, err, err)
				// need to close session
				state.curSession.Close()
			}
		} else if ev.newSession != nil {
			if state.curSession != nil && !state.curSession.closed {
				if err := state.curSession.SendPrivMsg(
					systemNick, state.tg.User().IRCNickname(),
					"New inbound IRC connection. Disconnect this session"); err != nil {
					logger.Error.Printf("%s %s", state.curSession, err)
				}
				state.curSession.Close()
			}
			state.curSession = ev.newSession
		}
	}
}

type IRCMsg string

func (s IRCMsg) Lines() []string { return strings.Split(string(s), "\n") }

type session struct {
	conn   net.Conn
	reader *bufio.Reader

	Nick     string
	Username string

	closed bool
}

func newSession(conn net.Conn) *session {
	return &session{
		conn: conn,
		// Create a buffered reader to read input from the client
		reader: bufio.NewReader(conn),

		Nick:     "-?-",
		Username: "-?-",
	}
}

func (s *session) Read() (string, error) {
	if s.closed {
		return "", errAlreadyClosed
	}
	return s.reader.ReadString('\n')
}

func (s *session) SendPrivMsg(sender string, recepient string, text string) error {
	msg := fmt.Sprintf(":%s PRIVMSG %s :%s", sender, recepient, text)
	if _, err := s.Write(IRCMsg(msg)); err != nil {
		return fmt.Errorf("SendPrivMsg : %w", err)
	}
	return nil
}

func (s *session) Writef(format string, a ...any) (int, error) {
	return s.Write(IRCMsg(fmt.Sprintf(format, a...)))
}

func (s *session) Write(msg ...IRCMsg) (int, error) {
	if s.closed {
		return 0, errAlreadyClosed
	}
	count := 0
	for _, m := range msg {
		m += "\n"
		logger.Info.Printf("%s sending %s", s, m)
		c, err := s.conn.Write([]byte(m))
		if err != nil {
			return count, err
		}
		count += c
	}
	return count, nil
}

func (s *session) Close() {
	if !s.closed {
		s.conn.Close()
		s.closed = true
	}
}

func (s *session) String() string {
	state := "open"
	if s.closed {
		state = "closed"
	}
	return fmt.Sprintf("%s %s!%s conn=%s",
		s.conn.RemoteAddr(), s.Nick, s.Username, state)
}

type CMD string

func (c CMD) is(command string) bool {
	return strings.HasPrefix(string(c), command)
}

func (c CMD) part(i int) string {
	toks := strings.Split(string(c), " ")
	if len(toks) < i+1 {
		return ""
	}
	return toks[i]
}

func (c CMD) tail() string {
	s := []rune(c)
	i, r := 0, ' '
	for i, r = range s {
		if i == 0 && r == ':' {
			continue
		}
		if r == ':' {
			break
		}
	}
	return string(s[i+1:])
}

func handleConnection(events chan *Event, sess *session) {
	defer sess.Close()

	events <- &Event{newSession: sess}
	for {
		rawMsg, err := sess.Read()
		if err != nil {
			if err == io.EOF {
				logger.Info.Printf("%s disconnected", sess)
				return
			}
			logger.Error.Printf("%s read from connection: %s", sess, err)
			return
		}
		events <- &Event{ircUpdate: rawMsg}
	}
}

func handleSystemReplies(state *State, cmd CMD) error {
	if cmd.tail() == "stats" {
		stats, err := state.tg.GetNetworkStatistics(context.Background(),
			&client.GetNetworkStatisticsRequest{OnlyCurrent: true})
		if err != nil {
			return err
		}
		for _, e := range stats.Entries {
			reply := fmt.Sprintf("Unknown stats: %T", e)
			switch ns := e.(type) {
			case *client.NetworkStatisticsEntryFile:
				reply = fmt.Sprintf("network_type=%s; sent_bytes=%d; received_bytes=%d",
					ns.NetworkType.NetworkTypeConstructor(), ns.SentBytes, ns.ReceivedBytes)
			}
			if err := state.curSession.SendPrivMsg(
				systemNick, state.tg.User().IRCNickname(), reply); err != nil {
				return fmt.Errorf("handleSystemReplies : %w", err)
			}
		}
		return nil
	}
	if err := state.curSession.SendPrivMsg(
		systemNick, state.tg.User().IRCNickname(), "System is up and running."); err != nil {
		return fmt.Errorf("handleSystemReplies : %w", err)
	}
	return nil
}

func handleIRCJoinToChannels(state *State, channels ...string) error {
	sess := state.curSession
	for _, cn := range channels {
		cn = strings.TrimSpace(cn)
		chat := state.joinChatByName(cn)
		if chat == nil {
			if _, err := sess.Writef(":%s 403 %s %s :No such channel",
				serverName, state.curSession.Nick, cn); err != nil {
				return fmt.Errorf("%w: write to connection:", err)
			}
			continue
		}
		replies := []IRCMsg{
			IRCMsg(fmt.Sprintf(":%s JOIN %s", sess.Nick, cn)),
			IRCMsg(fmt.Sprintf(":%s 332 %s %s :%s",
				serverName, sess.Nick, cn, chat.Topic())),
		}
		for _, m := range chat.Members() {
			replies = append(replies,
				IRCMsg(fmt.Sprintf(":%s 353 %s = %s :%s", serverName, sess.Nick, cn, m.IRCNickname())))
		}
		replies = append(replies,
			IRCMsg(fmt.Sprintf(":%s 353 %s = %s :%s", serverName, sess.Nick, cn, "@"+systemNick)))
		replies = append(replies, IRCMsg(fmt.Sprintf(":localhost 366 %s %s :End of /NAMES list.", sess.Nick, cn)))
		if _, err := sess.Write(replies...); err != nil {
			return fmt.Errorf("%w: write to connection:", err)
		}
	}
	return nil
}

func handleIRCPrivMessage(state *State, cmd CMD) error {
	rcpt := cmd.part(1)
	sess := state.curSession
	chat := state.joinedChats[rcpt]
	if chat == nil {
		if _, err := sess.Writef(":localhost 403 %s %s :No such recepient",
			state.curSession.Nick, rcpt); err != nil {
			return fmt.Errorf("%w: write to connection:", err)
		}
		return nil
	}
	ac, err := state.tg.GetChat(chat.Id)
	if err != nil {
		return err
	}
	if _, err := state.sendToTG(chat.Chat.Id, cmd.tail()); err != nil {
		return err
	}
	if ac.LastMessage != nil {
		if err := state.tg.ViewMessages(chat.Id, ac.LastMessage.Id); err != nil {
			// should not terminate this request
			logger.Error.Printf("tg.ViewMessages %e", err)
		}
	}
	return nil
}

func handleIRCCommand(state *State, msg string) error {
	sess := state.curSession
	command := CMD(strings.TrimSpace(msg))
	logger.Info.Printf("%s received: %s", sess, command)

	// Session init sequence:
	// C: USER username 0 * :Real Name
	// C: NICK mynickname
	// S: :irc.example.com 001 mynickname :Welcome to the IRC Network mynickname!username@host

	if command.is("NICK") {
		sess.Nick = command.part(1)
	} else if command.is("USER") {
		sess.Username = command.part(1)
		rpl := fmt.Sprintf(":%s 001 %s :Welcome to the Telegram to IRC bridge %s!%s@localhost",
			serverName, sess.Nick, sess.Nick, sess.Username)
		if _, err := sess.Write(IRCMsg(rpl)); err != nil {
			return fmt.Errorf("%w: write to connection:", err)
		}
		if err := state.curSession.SendPrivMsg(
			systemNick, state.tg.User().IRCNickname(), "Hello!"); err != nil {
			return fmt.Errorf("write to connection : %w", err)
		}
		// TODO: implement auto-join on connection
		// logger.Debug.Printf("chats: %d", len(state.chats))
		// for _, ch := range state.chats {
		// 	if err := handleIRCJoinToChannels(state, ch.ChannelName()); err != nil {
		// 		return err
		// 	}
		// }
	} else if command.is("PING") {
		if _, err := sess.Write("PONG :pong"); err != nil {
			return fmt.Errorf("%w: write to connection:", err)
		}
	} else if command.is("CAP LS") {
		rpl := ":" + serverName + " CAP * LS :"
		if _, err := sess.Write(IRCMsg(rpl)); err != nil {
			return fmt.Errorf("%w: write to connection:", err)
		}
	} else if command.is("CAP REQ") {
		// :irc.example.com CAP * NAK :multi-prefix
		rpl := ":" + serverName + " CAP * NAK"
		if _, err := sess.Write(IRCMsg(rpl)); err != nil {
			return fmt.Errorf("%w: write to connection:", err)
		}
	} else if command.is("LIST") {
		// 	obsolete in rfc281: ":localhost 321 mynickname Channel :Users  Name",
		// 	":localhost 322 mynickname #general 42 :General discussion channel",
		// 	":localhost 322 mynickname #random 15 :Random topics and fun",
		// 	":localhost 322 mynickname #help 5 :Get help and support",
		// 	":localhost 323 mynickname :End of /LIST",
		replies := make([]IRCMsg, 0, len(state.chats))
		for _, cht := range state.chats {
			replies = append(replies,
				IRCMsg(fmt.Sprintf(":%s 322 %s %s 0 :%s",
					serverName, state.curSession.Nick, cht.ChannelName(), cht.Topic())))
		}
		replies = append(replies,
			IRCMsg(fmt.Sprintf(":%s 323 %s :End of /LIST", serverName, state.curSession.Nick)))
		if _, err := sess.Write(replies...); err != nil {
			return fmt.Errorf("%w: write to connection:", err)
		}
	} else if command.is("MODE") {
		channelName := command.part(1)
		rpl := fmt.Sprintf(":%s MODE %s :", serverName, channelName)
		if _, err := sess.Write(IRCMsg(rpl)); err != nil {
			return fmt.Errorf("%w: write to connection:", err)
		}
	} else if command.is("CAP END") {
		logger.Info.Printf("%s %s", sess, command)
	} else if command.is("QUIT") {
		logger.Info.Printf("%s disconnected", sess)
	} else if command.is("PART ") {
		channelName := command.part(1)
		logger.Info.Printf("%s parted %s", sess, channelName)
		// :alice PART #general
		rpl := fmt.Sprintf(":%s PART %s", sess.Nick, channelName)
		if _, err := sess.Write(IRCMsg(rpl)); err != nil {
			return fmt.Errorf("%w: write to connection:", err)
		}
	} else if command.is("JOIN") {
		channels := strings.Split(command.part(1), ",")
		if err := handleIRCJoinToChannels(state, channels...); err != nil {
			return fmt.Errorf("%w: handleIRCJoinToChannels:", err)
		}
	} else if command.is("PRIVMSG") {
		rcpt := command.part(1)
		if rcpt == systemNick {
			return handleSystemReplies(state, command)
		}
		return handleIRCPrivMessage(state, command)
	} else {
		logger.Info.Printf("%s Unknown command: %s", sess, command)
		rpl := fmt.Sprintf(":%s 421 %s %s :Unknown or unsupported command",
			serverName, sess.Nick, command.part(0))
		if _, err := sess.Write(IRCMsg(rpl)); err != nil {
			return fmt.Errorf("%w: write to connection:", err)
		}
	}
	return nil
}

func dieIf(err error) {
	if err != nil {
		log.Fatalf("fatal: %T '%s'", err, err)
	}
}
