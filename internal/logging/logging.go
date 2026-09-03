package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

type Entry struct {
	Timestamp int64          `json:"timestamp"`
	Level     string         `json:"level"`
	Message   string         `json:"message"`
	Fields    map[string]any `json:"fields,omitempty"`
}

type Ring struct {
	mu      sync.RWMutex
	entries []Entry
	limit   int
}

func New(path string) (*slog.Logger, *Ring, io.Closer, error) {
	ring := &Ring{entries: make([]Entry, 0, 500), limit: 500}
	var writer io.Writer = os.Stdout
	var closer io.Closer = io.NopCloser(nilReader{})
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, nil, nil, err
		}
		file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, nil, err
		}
		writer = io.MultiWriter(os.Stdout, file)
		closer = file
	}
	jsonHandler := slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(&teeHandler{handlers: []slog.Handler{jsonHandler, &ringHandler{ring: ring}}})
	return logger, ring, closer, nil
}

func (r *Ring) List(limit int) []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if limit <= 0 || limit > r.limit {
		limit = r.limit
	}
	if limit > len(r.entries) {
		limit = len(r.entries)
	}
	result := make([]Entry, limit)
	for index := 0; index < limit; index++ {
		result[index] = r.entries[len(r.entries)-1-index]
	}
	return result
}

func (r *Ring) append(entry Entry) {
	r.mu.Lock()
	if len(r.entries) == r.limit {
		copy(r.entries, r.entries[1:])
		r.entries[len(r.entries)-1] = entry
	} else {
		r.entries = append(r.entries, entry)
	}
	r.mu.Unlock()
}

type ringHandler struct {
	ring   *Ring
	attrs  []slog.Attr
	groups []string
}

func (h *ringHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (h *ringHandler) Handle(_ context.Context, record slog.Record) error {
	fields := make(map[string]any)
	for _, attr := range h.attrs {
		addAttr(fields, h.groups, attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		addAttr(fields, h.groups, attr)
		return true
	})
	h.ring.append(Entry{
		Timestamp: record.Time.UnixMilli(),
		Level:     record.Level.String(),
		Message:   record.Message,
		Fields:    fields,
	})
	return nil
}

func (h *ringHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &clone
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	clone := *h
	clone.groups = append(append([]string(nil), h.groups...), name)
	return &clone
}

type teeHandler struct{ handlers []slog.Handler }

func (h *teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (h *teeHandler) Handle(ctx context.Context, record slog.Record) error {
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, record.Level) {
			if err := handler.Handle(ctx, record.Clone()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(h.handlers))
	for index, handler := range h.handlers {
		handlers[index] = handler.WithAttrs(attrs)
	}
	return &teeHandler{handlers: handlers}
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(h.handlers))
	for index, handler := range h.handlers {
		handlers[index] = handler.WithGroup(name)
	}
	return &teeHandler{handlers: handlers}
}

func addAttr(fields map[string]any, groups []string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	key := attr.Key
	if len(groups) > 0 {
		for _, group := range groups {
			if group != "" {
				key = group + "." + key
			}
		}
	}
	fields[key] = attr.Value.Any()
}

type nilReader struct{}

func (nilReader) Read([]byte) (int, error) { return 0, io.EOF }
