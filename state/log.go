package state

import (
	"context"
	"log/slog"
)

type stateLogHandler struct {
	handler slog.Handler
}

func SetLogHandler(handler slog.Handler) {
	logHandler.handler = handler
}

var logHandler *stateLogHandler = &stateLogHandler{handler: slog.Default().Handler()}

var logKeys = [...]stateCtxKey{moduleCtxKey, triggerIdCtxKey, hookCtxKey}

func (h *stateLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if noLog, ok := ctx.Value(logFlagDoNotLog).(bool); ok && noLog {
		return false
	}
	return h.handler.Enabled(ctx, level)
}

func (h *stateLogHandler) Handle(ctx context.Context, record slog.Record) error {
	for _, k := range logKeys {
		if v, ok := ctx.Value(k).(slog.LogValuer); ok {
			record.AddAttrs(slog.Attr{Key: string(k), Value: v.LogValue()})
		}
	}
	return h.handler.Handle(ctx, record)
}

func (h *stateLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.handler.WithAttrs(attrs)
}

func (h *stateLogHandler) WithGroup(name string) slog.Handler {
	return h.handler.WithGroup(name)
}

var log *slog.Logger = slog.New(logHandler)

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
