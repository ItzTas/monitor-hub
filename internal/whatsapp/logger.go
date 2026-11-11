package whatsapp

import (
	"fmt"
	"log/slog"
	"regexp"

	waLog "go.mau.fi/whatsmeow/util/log"
)

var longNumber = regexp.MustCompile(`\d{7,}`)

type logger struct {
	log    *slog.Logger
	module string
}

func newLogger(log *slog.Logger, module string) waLog.Logger {
	return logger{log: log, module: module}
}

func (l logger) Errorf(msg string, args ...any) {
	l.log.Error(redact(fmt.Sprintf(msg, args...)), "module", l.module)
}

func (l logger) Warnf(msg string, args ...any) {
	l.log.Warn(redact(fmt.Sprintf(msg, args...)), "module", l.module)
}

func (l logger) Infof(msg string, args ...any) {
	l.log.Debug(redact(fmt.Sprintf(msg, args...)), "module", l.module)
}

func (l logger) Debugf(string, ...any) {}

func (l logger) Sub(module string) waLog.Logger {
	return logger{log: l.log, module: l.module + "/" + module}
}

func redact(text string) string {
	return longNumber.ReplaceAllString(text, "[redacted]")
}
