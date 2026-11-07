package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// rollingWriter is a minimal size-based rotating log file.
type rollingWriter struct {
	mu   sync.Mutex
	path string
	max  int64
	keep int
	f    *os.File
	size int64
}

func newRollingWriter(path string, max int64, keep int) (*rollingWriter, error) {
	w := &rollingWriter{path: path, max: max, keep: keep}
	return w, w.open()
}

func (w *rollingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, _ := f.Stat()
	w.f, w.size = f, st.Size()
	return nil
}

func (w *rollingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size+int64(len(p)) > w.max {
		w.f.Close()
		for i := w.keep - 1; i >= 1; i-- {
			os.Rename(fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1))
		}
		os.Rename(w.path, w.path+".1")
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// multiHandler fans a record out to several slog handlers.
type multiHandler []slog.Handler

func (m multiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (m multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r.Clone())
		}
	}
	return nil
}

func (m multiHandler) WithAttrs(a []slog.Attr) slog.Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithAttrs(a)
	}
	return out
}

func (m multiHandler) WithGroup(n string) slog.Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithGroup(n)
	}
	return out
}

func setupLogging(cfg Config) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	handlers := multiHandler{slog.NewTextHandler(os.Stdout, opts)}

	dir := filepath.Join(cfg.CacheDir, "logs")
	if err := os.MkdirAll(dir, 0o755); err == nil {
		if rw, err := newRollingWriter(filepath.Join(dir, "server.log"), 10<<20, 5); err == nil {
			handlers = append(handlers, slog.NewJSONHandler(rw, opts))
		}
	}
	return slog.New(handlers)
}
