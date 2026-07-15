package irc

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/nchern/tgirc/pkg/logger"
)

// ErrAlreadyClosed gets returned on attempt to use a sessions with closed the
// underline network connection
var ErrAlreadyClosed = errors.New("connection closed")

type Msg string

func (s Msg) Lines() []string { return strings.Split(string(s), "\n") }

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
	msg := fmt.Sprintf(":%s PRIVMSG %s :%s", sender, recepient, text)
	if _, err := s.Write(Msg(msg)); err != nil {
		return fmt.Errorf("SendPrivMsg : %w", err)
	}
	return nil
}

// Writef writes a formatted IRC message to this session
func (s *Session) Writef(format string, a ...any) (int, error) {
	return s.Write(Msg(fmt.Sprintf(format, a...)))
}

// Writef writes IRC messages to this session
func (s *Session) Write(msg ...Msg) (int, error) {
	if s.closed {
		return 0, ErrAlreadyClosed
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

// CMD represents an IRC command
type CMD string

// Is tests this command
func (c CMD) Is(command string) bool {
	return strings.HasPrefix(string(c), command)
}

// Part returns a specified part of this command according to IRC standard
func (c CMD) Part(i int) string {
	toks := strings.Split(string(c), " ")
	if len(toks) < i+1 {
		return ""
	}
	return toks[i]
}

// Tail returns a tail of this IRC command according to IRC standard
func (c CMD) Tail() string {
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
