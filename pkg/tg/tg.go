package tg

import (
	"fmt"
	"strings"
	"time"

	"github.com/zelenin/go-tdlib/client"
)

func slugify(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, " ", "_"),
		"\t", "_")
}

// Session is a convenience wrapper of a Telegram clien
// to provide saner API shortcuts
type Session struct {
	*client.Client

	user *User
}

// NewSession creates a new Telegram session
func NewSession(c *client.Client, user *User) *Session {
	return &Session{c, user}
}

// User returns a currently logged-in tg user
func (tg *Session) User() *User { return tg.user }

func (tg *Session) ViewMessages(chatID int64, messageIDs ...int64) error {
	_, err := tg.Client.ViewMessages(&client.ViewMessagesRequest{
		// XXX: Forcing to mark it as read is a temporary hack.
		// The proper logic involves opening / closing chats
		// which is unclear how to do as of now
		// More details: https://github.com/tdlib/td/issues/46
		ForceRead:  true,
		ChatId:     chatID,
		MessageIds: messageIDs,
	})
	return err
}

func (tg *Session) GetUser(userID int64) (*User, error) {
	u, err := tg.Client.GetUser(&client.GetUserRequest{UserId: userID})
	if err != nil {
		return nil, err
	}
	return &User{u}, nil
}

func (tg *Session) GetChat(chatID int64) (*Chat, error) {
	ct, err := tg.Client.GetChat(&client.GetChatRequest{ChatId: chatID})
	if err != nil {
		return nil, err
	}
	return NewChat(ct), nil
}

// Send sends a text message to Telegram
func (tg *Session) Send(chatID int64, text string) (*client.Message, error) {
	return tg.SendMessage(&client.SendMessageRequest{
		ChatId: chatID,
		InputMessageContent: &client.InputMessageText{
			Text: &client.FormattedText{
				Text: text,
			},
		}})
}

// User is a conviniece wrapper for Telegram user
type User struct {
	*client.User
}

func NewUser(u *client.User) *User { return &User{u} }

// PrimaryUsername inferes a primary name of this user
func (u *User) PrimaryUsername() string {
	if u.Usernames != nil && len(u.Usernames.ActiveUsernames) > 0 {
		return u.Usernames.ActiveUsernames[0]
	}
	return fmt.Sprintf("%d", u.Id)
}

// IRCNickname returns a nick sutable for IRC
func (u *User) IRCNickname() string {
	return slugify(u.PrimaryUsername())
}

// Message provides a complete and offline message data structure that
// encapsulates User that sent this message and a parent chat
// Call it MessageView?
type Message struct {
	*client.Message

	Sender *User
	Chat   *client.Chat

	Link string
}

func NewMessage(m *client.Message, chat *client.Chat) *Message {
	return &Message{Message: m, Chat: chat}
}

// Permissions is a shortcut to retrieve this message chat permissions.
// It also simplifies nil checks.
func (m *Message) Permissions() *client.ChatPermissions {
	if m.Chat == nil || m.Chat.Permissions == nil {
		return &client.ChatPermissions{}
	}
	return m.Chat.Permissions
}

// UniqueID uniquely identifies this message across all / messages / chats
func (m *Message) UniqueID() string {
	return fmt.Sprintf("%d.%d", m.ChatId, m.Id)
}

// DateAsRFC1123Z returns message date in RFC1123Z format
func (m *Message) DateAsRFC1123Z() string {
	return time.Unix(int64(m.Date), 0).Format(time.RFC1123Z)
}

// Seen tells if this message has been already seen by the current user
// TDLib logic explained here: https://github.com/tdlib/td/issues/1878
// func (m *Message) Seen(curUserID int) bool {
// 	fromThisUser := m.User != nil && m.User.Id == curUserID
// 	return fromThisUser || m.Id <= m.Chat.LastReadInboxMessageId
// }

// FromDisplayName returns a best-effort display name of a sender
func (m *Message) FromDisplayName() string {
	if m.Sender != nil {
		res := strings.TrimSpace(fmt.Sprintf("%s %s",
			m.Sender.FirstName, m.Sender.LastName))
		if res != "" {
			return res
		}
		if m.Sender.Usernames != nil && len(m.Sender.Usernames.ActiveUsernames) > 0 {
			return m.Sender.Usernames.ActiveUsernames[0]
		}
	}
	return m.Chat.Title
}

func fetchPhotoNames(p *client.Photo) []string {
	names := []string{}
	if p == nil {
		return names
	}
	for _, fs := range p.Sizes {
		if fs == nil {
			continue
		}
		names = append(names, fs.Photo.Local.Path)
	}
	return names
}

// Text returns this message text
func (m *Message) Text() string {
	switch mt := m.Content.(type) {
	case *client.MessageText:
		if mt.Text == nil {
			return "<empty>"
		}
		return mt.Text.Text
	case *client.MessagePhoto:
		caption := "<no-caption>"
		if mt.Caption != nil {
			caption = mt.Caption.Text
		}
		names := fetchPhotoNames(mt.Photo)
		return caption + fmt.Sprintf(" <photo> %s", strings.Join(names, "; "))
	case *client.MessageVideo:
		// TODO: add nil checks
		return mt.Caption.Text + fmt.Sprintf(" <video> %s", mt.Video.FileName)
	case *client.MessageDocument:
		return fmt.Sprintf("%s <file upload> mime=%s; filename=%s",
			mt.Caption.Text, mt.Document.MimeType, mt.Document.FileName)
	case *client.MessageSticker:
		if mt.Sticker != nil && mt.Sticker.Emoji != "" {
			return mt.Sticker.Emoji
		}
		return "<unknown-sticker>"
	default:
		return fmt.Sprintf("Unknown message: <%s>", m.Content.MessageContentType())
	}
}

// FirstLine returns a first lime of this message text
func (m *Message) FirstLine() string {
	lines := strings.Split(m.Text(), "\n")
	if len(lines) > 0 {
		return lines[0]
	}
	return ""
}

// SenderID returns this message sender id
func (m *Message) SenderID() int64 {
	if u, ok := m.SenderId.(*client.MessageSenderUser); ok {
		return u.UserId
	}
	if c, ok := m.SenderId.(*client.MessageSenderChat); ok {
		return c.ChatId
	}
	panic("Unknown MessageSenderType: " + m.SenderId.MessageSenderType())
}

// Chat is a convenience wrapper for client.Chat struc
type Chat struct {
	*client.Chat

	members []*User
}

// NewChat creates a new instance of Chat
func NewChat(chat *client.Chat) *Chat {
	return &Chat{Chat: chat}
}

func (ch *Chat) Members() []*User {
	return ch.members
}

// SetMembers sets members of this chat
func (ch *Chat) SetMembers(members []*User) *Chat {
	ch.members = members
	return ch
}

func (ch *Chat) Topic() string {
	return fmt.Sprintf("%s (%s)", ch.Title, ch.Type.ChatTypeType())
}

func (ch *Chat) ChannelName() string {
	res := strings.TrimSpace(slugify(ch.Title))
	if res == "" {
		res = fmt.Sprintf("%d", ch.Id)
	}
	return "#" + res
}

func (ch *Chat) Supported() bool {
	switch ch.Type.(type) {
	case *client.ChatTypePrivate, *client.ChatTypeBasicGroup:
		return true
	default:
		return false
	}
}
