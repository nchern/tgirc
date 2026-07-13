package logger

import (
	"fmt"
	"log"
	"os"
)

const defaultLogFlags = log.LstdFlags | log.Llongfile | log.Lmsgprefix | log.LUTC

var (
	// Error represents a log sink for error messages
	Error Interface

	// Info represents a log sink for info messages
	Info Interface

	// Debug represents a log sink for debug messages
	Debug Interface

	nullLoggerImpl = &nullLogger{}
)

func init() {
	log.SetFlags(defaultLogFlags)

	Error = newErrorLogger()
	Info = log.New(os.Stderr, "INFO ", inferLogFlags())
	Debug = log.New(os.Stderr, "DEBUG ", inferLogFlags())
}

// Null returns null logger implementation
func Null() Interface { return nullLoggerImpl }

// Interface abstracts logging interface
type Interface interface {
	Println(v ...interface{})
	Printf(format string, v ...interface{})
}

type nullLogger struct{}

func (l *nullLogger) Println(v ...interface{}) {}

func (l *nullLogger) Printf(format string, v ...interface{}) {}

type errorLogger struct {
	log *log.Logger
}

func newErrorLogger() *errorLogger {
	return &errorLogger{
		log: log.New(os.Stderr, "ERROR ", inferLogFlags()),
	}
}

func (l *errorLogger) Println(v ...interface{}) {
	l.log.Output(2, fmt.Sprintln(v...))
}

func (l *errorLogger) Printf(format string, v ...interface{}) {
	for _, it := range v {
		if err, ok := it.(error); ok {
			format = fmt.Sprintf("err_type=%T ", err) + format
		}
	}
	l.log.Output(3, fmt.Sprintf(format, v...))
}

// isTimestampDisabled allows to remove timestamps in logs if env variable LOG_DISABLE_TIMESTAMP is set
// Use case: a service is run under an external log collector that already appends
//
//	timestamps, e.g. systemd supplying logs to journald
func isTimestampDisabled() bool { return os.Getenv("LOG_DISABLE_TIMESTAMP") != "" }

func inferLogFlags() int {
	if isTimestampDisabled() {
		return log.Llongfile | log.Lmsgprefix
	}
	return defaultLogFlags
}
