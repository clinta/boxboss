package state

import (
	"context"
	"log/slog"
)

type StateLogHandler struct {
	handler slog.Handler
}

func (h *StateLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if noLog, ok := ctx.Value(logFlagDoNotLog).(bool); ok && noLog {
		return false
	}
	return h.handler.Enabled(ctx, level)
}

func (h *StateLogHandler) Handle(ctx context.Context, record slog.Record) error {
	// TODO: Add fields based on context
	return h.handler.Handle(ctx, record)
}

func (h *StateLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.handler.WithAttrs(attrs)
}

func (h *StateLogHandler) WithGroup(name string) slog.Handler {
	return h.handler.WithGroup(name)
}

var stateLogHandler *StateLogHandler = &StateLogHandler{handler: slog.Default().Handler()}

func SetHandler(handler slog.Handler) {
	stateLogHandler.handler = handler
}

var log *slog.Logger = slog.New(stateLogHandler)

func Log() *slog.Logger {
	return log
}

type stateLogCtxFlag uint

const (
	logFlagDoNotLog stateLogCtxFlag = iota
)

func applyCtxTransforms(ctx context.Context, transforms ...func(context.Context) context.Context) context.Context {
	for _, f := range transforms {
		ctx = f(ctx)
	}
	return ctx
}

func setDoNotLog(ctx context.Context) context.Context {
	return context.WithValue(ctx, logFlagDoNotLog, true)
}

func setDoLog(ctx context.Context) context.Context {
	return context.WithValue(ctx, logFlagDoNotLog, false)
}
