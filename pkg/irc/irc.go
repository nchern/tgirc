package irc

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net"

	ircproto "gopkg.in/irc.v3"
)

// ErrAlreadyClosed gets returned on attempt to use a sessions with closed the
// underline network connection
var ErrAlreadyClosed = errors.New("connection closed")

// FormatMessage formats an IRC message with an optional prefix.
func FormatMessage(prefix, command string, params ...string) string {
	msg := &ircproto.Message{Command: command, Params: params}
	if prefix != "" {
		msg.Prefix = &ircproto.Prefix{Name: prefix}
	}
	return msg.String()
}

// Session represents IRC session
type Session struct {
	conn   net.Conn
	reader *bufio.Reader

	Nick     string
	Username string

	closed bool
}

// NewSession returns a new IRC session
func NewSession(conn net.Conn) *Session {
	return &Session{
		conn: conn,
		// Create a buffered reader to read input from the client
		reader: bufio.NewReader(conn),

		Nick:     "-?-",
		Username: "-?-",
	}
}

// NewEmptySession returns an empty closed session
func NewEmptySession() *Session { return &Session{closed: true} }

// Closed tells if this session is closed
func (s *Session) Closed() bool { return s.closed }

// Read reads a string from this session
func (s *Session) Read() (string, error) {
	if s.closed {
		return "", ErrAlreadyClosed
	}
	return s.reader.ReadString('\n')
}

// SendPrivMsg sends a private message to this session
func (s *Session) SendPrivMsg(sender string, recepient string, text string) error {
	msg := &ircproto.Message{
		Prefix:  &ircproto.Prefix{Name: sender},
		Command: "PRIVMSG",
		Params:  []string{recepient, text},
	}
	if _, err := s.Write(msg.String()); err != nil {
		return fmt.Errorf("SendPrivMsg : %w", err)
	}
	return nil
}

// Writef writes a formatted IRC message to this session
func (s *Session) Writef(format string, a ...any) (int, error) {
	return s.Write(fmt.Sprintf(format, a...))
}

// Write writes IRC messages to this session
func (s *Session) Write(msg ...string) (int, error) {
	if s.closed {
		return 0, ErrAlreadyClosed
	}
	count := 0
	for _, m := range msg {
		m += "\n"
		slog.Info("sending", "session", s, "message", m)
		c, err := s.conn.Write([]byte(m))
		if err != nil {
			return count, err
		}
		count += c
	}
	return count, nil
}

// Close closes this session
func (s *Session) Close() {
	if !s.closed {
		s.conn.Close()
		s.closed = true
	}
}

// String returns a string representation of this session
func (s *Session) String() string {
	state := "open"
	if s.closed {
		state = "closed"
	}
	return fmt.Sprintf("%s %s!%s conn=%s",
		s.conn.RemoteAddr(), s.Nick, s.Username, state)
}
