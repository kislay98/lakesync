package engine

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"lakesync/internal/core"
	"lakesync/internal/store"
)

// plog writes pipeline logs to the database (for the UI) and to the process log.
type plog struct {
	st         *store.Store
	log        *slog.Logger
	pipelineID string
	collection string
}

func (l *plog) write(ctx context.Context, level, msg string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	attrs := []any{"pipeline", l.pipelineID, "collection", l.collection}
	for k, v := range fields {
		if s, ok := v.(string); ok {
			v = core.RedactURI(s)
			fields[k] = v
		}
		attrs = append(attrs, k, v)
	}
	msg = core.RedactURI(msg)
	switch level {
	case "debug":
		l.log.Debug(msg, attrs...)
	case "warn":
		l.log.Warn(msg, attrs...)
	case "error":
		l.log.Error(msg, attrs...)
	default:
		l.log.Info(msg, attrs...)
	}
	if level == "debug" {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	id, err := l.st.AppendLog(wctx, l.pipelineID, l.collection, level, msg, fields)
	if err != nil {
		return
	}
	_ = l.st.Notify(wctx, store.Event{Type: "log", PipelineID: l.pipelineID, Collection: l.collection, Data: jsonRaw(store.LogEntry{ID: id, PipelineID: l.pipelineID, Collection: l.collection, Level: level, Message: msg, Fields: jsonRaw(fields), At: time.Now()})})
}

func (l *plog) Info(ctx context.Context, msg string, fields map[string]any) {
	l.write(ctx, "info", msg, fields)
}
func (l *plog) Warn(ctx context.Context, msg string, fields map[string]any) {
	l.write(ctx, "warn", msg, fields)
}
func (l *plog) Error(ctx context.Context, msg string, fields map[string]any) {
	l.write(ctx, "error", msg, fields)
}
func (l *plog) Debug(ctx context.Context, msg string, fields map[string]any) {
	l.write(ctx, "debug", msg, fields)
}

func jsonRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
