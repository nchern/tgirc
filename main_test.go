package main

import (
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/nchern/tgirc/pkg/irc"
	"github.com/nchern/tgirc/pkg/tg"
	"github.com/stretchr/testify/assert"
	"github.com/zelenin/go-tdlib/client"
)

var tgLoggedIn = tg.NewUser(&client.User{Id: 42})

type testAddr struct{}

func (a *testAddr) Network() string { return "test-network" }
func (a *testAddr) String() string  { return "127.0.0.1" }

type mockNetConn struct {
	readIdx  int
	incoming []string

	written []string

	closed bool
}

func newMockConn() *mockNetConn {
	return &mockNetConn{
		written:  []string{},
		incoming: []string{},
	}
}

// Read reads data from the connection.
func (c *mockNetConn) Read(b []byte) (n int, err error) {
	slog.Debug("Read:", "n", len(b))
	if c.readIdx >= len(c.incoming) {
		return 0, io.EOF
	}
	read := []byte(c.incoming[c.readIdx] + "\n")
	if len(b) < len(read) {
		// XXX: assumes b has enough space to consume the whole string
		panic("not implemented: not enough buffer")
	}
	c.readIdx++
	copy(b, read)
	return len(read), nil
}

// Write writes data to the connection.
func (c *mockNetConn) Write(b []byte) (n int, err error) {
	c.written = append(c.written, string(b))
	return len(b), nil
}

// Close closes the connection.
func (c *mockNetConn) Close() error {
	c.closed = true
	return nil
}

// LocalAddr returns the local network address, if known.
func (c *mockNetConn) LocalAddr() net.Addr {
	panic("Not implemented")
}

// RemoteAddr returns the remote network address, if known.
func (c *mockNetConn) RemoteAddr() net.Addr { return &testAddr{} }

// SetDeadline sets the read and write deadlines associated
// with the connection. It is equivalent to calling both
// SetReadDeadline and SetWriteDeadline.
func (c *mockNetConn) SetDeadline(t time.Time) error {
	panic("Not implemented")
}

// SetReadDeadline sets the deadline for future Read calls
// and any currently-blocked Read call.
func (c *mockNetConn) SetReadDeadline(t time.Time) error {
	panic("Not implemented")
}

// SetWriteDeadline sets the deadline for future Write calls
func (c *mockNetConn) SetWriteDeadline(t time.Time) error {
	panic("Not implemented")
}

type mockTGSession struct {
	chats map[int64]*tg.Chat

	sent []*client.Message
}

func newMockTGSession() *mockTGSession {
	return &mockTGSession{
		chats: map[int64]*tg.Chat{},
		sent:  []*client.Message{},
	}
}

func (t *mockTGSession) addChats(chats ...*tg.Chat) *mockTGSession {
	for _, ch := range chats {
		t.chats[ch.Id] = ch
	}
	return t
}

func (t *mockTGSession) GetBasicGroupFullInfo(
	req *client.GetBasicGroupFullInfoRequest) (*client.BasicGroupFullInfo, error) {
	panic("Not implemented")
}

func (t *mockTGSession) GetChat(chatID int64) (*tg.Chat, error) {
	return t.chats[chatID], nil
}

func (t *mockTGSession) GetChats(*client.GetChatsRequest) (*client.Chats, error) {
	panic("Not implemented")
}

func (t *mockTGSession) GetNetworkStatistics(
	*client.GetNetworkStatisticsRequest) (*client.NetworkStatistics, error) {

	panic("Not implemented")
}

func (t *mockTGSession) GetUser(userID int64) (*tg.User, error) {
	panic("Not implemented")
}

func (t *mockTGSession) Send(chatID int64, text string) (*client.Message, error) {
	if t.chats[chatID] == nil {
		panic("chat not found")
	}
	t.sent = append(t.sent, &client.Message{
		Id:     int64(len(t.sent) + 1),
		ChatId: chatID,
		Content: &client.MessageText{
			Text: &client.FormattedText{Text: text},
		},
	})
	return t.sent[len(t.sent)-1], nil
}

func (t *mockTGSession) User() *tg.User {
	return tgLoggedIn
}

func (t *mockTGSession) ViewMessages(chatID int64, messageIDs ...int64) error {
	panic("Not implemented")
}

func playEventsOnMainLoop(conn net.Conn, state *State, events ...*Event) {
	sess := irc.NewSession(conn)

	in := make(chan *Event, len(events)+1)
	in <- &Event{newSession: sess}
	for _, e := range events {
		in <- e
	}
	close(in)

	mainEventLoop(state, in)
}

func mkChat(id int64, title string, lastMessageDate int32) *tg.Chat {
	tp := &client.ChatTypePrivate{}
	tp.Type = client.TypeChatTypePrivate
	chat := &client.Chat{Id: id, Title: title, Type: tp}
	if lastMessageDate > 0 {
		chat.LastMessage = &client.Message{Date: lastMessageDate}
	}
	return &tg.Chat{
		Chat: chat,
	}
}

func TestHandleIRCEventsShouldProcess(t *testing.T) {
	defaultChats := []*tg.Chat{mkChat(1, "test-chat", 0)}
	listChats := []*tg.Chat{
		mkChat(11, "narrow alpha", 3),
		mkChat(12, "broad beta", 2),
		mkChat(13, "narrow gamma", 1),
	}
	var tests = []struct {
		name     string
		expected []string
		given    string
		chats    []*tg.Chat
	}{
		{"unsupported command",
			[]string{":localhost 421 -?- abc :Unknown or unsupported command\n"},
			"abc",
			defaultChats},
		{"ping",
			[]string{"PONG :pong\n"},
			"PING localhost",
			defaultChats},
		{"cap ls",
			[]string{":localhost CAP * LS :\n"},
			"CAP LS 302",
			defaultChats},
		{"mode",
			[]string{":localhost 324 MODE -?- #test-chat +\n"},
			"MODE #test-chat",
			defaultChats},
		{"list without filter",
			[]string{
				":localhost 322 -?- #narrow_alpha 0 :narrow alpha (chatTypePrivate)\n",
				":localhost 322 -?- #broad_beta 0 :broad beta (chatTypePrivate)\n",
				":localhost 322 -?- #narrow_gamma 0 :narrow gamma (chatTypePrivate)\n",
				":localhost 323 -?- :End of /LIST\n",
			},
			"LIST",
			listChats},
		{"list invalid filter",
			[]string{":localhost 461 -?- LIST :Invalid filter\n"},
			"LIST [",
			listChats},
		{"list with filter",
			[]string{
				":localhost 322 -?- #narrow_alpha 0 :narrow alpha (chatTypePrivate)\n",
				":localhost 322 -?- #narrow_gamma 0 :narrow gamma (chatTypePrivate)\n",
				":localhost 323 -?- :End of /LIST\n",
			},
			"LIST narrow",
			listChats},
		{"list with no matches",
			[]string{":localhost 323 -?- :End of /LIST\n"},
			"LIST missing",
			listChats},
		{"user",
			[]string{
				":localhost 001 -?- :Welcome to the Telegram to IRC bridge -?-!usr@localhost\n",
				":SysServ PRIVMSG 42 :Hello!\n",
				":-?- JOIN #test-chat\n",
				":localhost 332 -?- #test-chat :test-chat (chatTypePrivate)\n",
				":localhost 353 -?- = #test-chat :@SysServ\n",
				":localhost 366 -?- #test-chat :End of /NAMES list.\n",
			},
			"USER usr 0 * :usr",
			defaultChats},
		{"private message to Sys user",
			[]string{":SysServ PRIVMSG 42 :System is up and running.\n"},
			"PRIVMSG SysServ :test",
			defaultChats},
		{"part",
			[]string{":-?- PART #channel-name\n"},
			"PART #channel-name :WeeChat 4.2.1",
			defaultChats},
		{"cap req",
			[]string{":localhost CAP * NAK\n"},
			"CAP REQ :",
			defaultChats},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			state := NewState(newMockTGSession())

			for _, ch := range tt.chats {
				state.chats[ch.Id] = ch
			}
			conn := newMockConn()

			playEventsOnMainLoop(conn, state,
				&Event{ircUpdate: tt.given})

			assert.NotNil(t, state.irc)
			assert.False(t, state.irc.Closed())
			assert.Equal(t, tt.expected, conn.written)
		})
	}
}

func TestHandleIRCNickCommandShouldSetNickOnSession(t *testing.T) {
	conn := newMockConn()
	state := NewState(nil)

	const expected = "test-nick"
	playEventsOnMainLoop(conn, state,
		&Event{ircUpdate: "NICK " + expected})

	assert.Len(t, conn.written, 0)
	assert.False(t, state.irc.Closed())
	assert.Equal(t, expected, state.irc.Nick)
}

func TestShouldDropCurrentConnectionOnNewIncoming(t *testing.T) {
	curConn := newMockConn()
	newSess := irc.NewSession(newMockConn())
	tgSess := newMockTGSession()

	state := NewState(tgSess)

	playEventsOnMainLoop(curConn, state,
		&Event{newSession: newSess})

	expected := []string{":SysServ PRIVMSG 42 :New inbound IRC connection. Disconnect this session\n"}
	assert.Equal(t, expected, curConn.written)
	assert.Equal(t, newSess, state.irc)
	assert.True(t, curConn.closed)
	assert.False(t, newSess.Closed())
}

func TestHandleIRCPrivMessageShould(t *testing.T) {
	chat := tg.NewChat(&client.Chat{
		Id:    11,
		Title: "John Doe",
		Type:  &client.ChatTypePrivate{},
	})
	var tests = []struct {
		name                string
		expectedIRCResponse []string
		expectedTGSentMsg   []*client.Message
		given               string
	}{
		{"reply if chat is unknown",
			[]string{":localhost 403 -?- #unknown_chat :No such recepient\n"},
			[]*client.Message{},
			"PRIVMSG #unknown_chat :hello !"},
		{"send message to channel",
			[]string{},
			[]*client.Message{
				{
					Id:     1,
					ChatId: chat.Id,
					Content: &client.MessageText{
						Text: &client.FormattedText{Text: "hello !"},
					},
				},
			},
			"PRIVMSG #John_Doe :hello !"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			tgSess := newMockTGSession()
			state := NewState(tgSess)

			// prepare state
			state.registerChat(chat)
			state.joinChatByName(chat.ChannelName())
			tgSess.addChats(chat)

			conn := newMockConn()
			playEventsOnMainLoop(conn, state,
				&Event{ircUpdate: tt.given})

			assert.NotNil(t, state.irc)
			assert.False(t, state.irc.Closed())
			assert.Equal(t, tt.expectedTGSentMsg, tgSess.sent)
			assert.Equal(t, tt.expectedIRCResponse, conn.written)
		})
	}
}
