package bossbox

import (
	"context"
	"log/slog"
)

type logHandler struct {
	handler slog.Handler
}

func SetLogHandler(handler slog.Handler) {
	bbLogHandler.handler = handler
}

var bbLogHandler *logHandler = &logHandler{handler: slog.Default().Handler()}

var logKeys = [...]ctxKey{moduleCtxKey, triggerIdCtxKey, hookCtxKey}

func (h *logHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if noLog, ok := ctx.Value(logFlagDoNotLog).(bool); ok && noLog {
		return false
	}
	return h.handler.Enabled(ctx, level)
}

func (h *logHandler) Handle(ctx context.Context, record slog.Record) error {
	for _, k := range logKeys {
		if v, ok := ctx.Value(k).(slog.LogValuer); ok {
			record.AddAttrs(slog.Attr{Key: string(k), Value: v.LogValue()})
		}
	}
	return h.handler.Handle(ctx, record)
}

func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.handler.WithAttrs(attrs)
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	return h.handler.WithGroup(name)
}

var log *slog.Logger = slog.New(bbLogHandler)

func Log() *slog.Logger {
	return log
}

type logCtxFlag uint

const (
	logFlagDoNotLog logCtxFlag = iota
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
