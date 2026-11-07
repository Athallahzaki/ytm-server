package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

type Server struct {
	cfg Config
	db  *sql.DB
	ytm *YTM
	sem chan struct{} // limits concurrent transcodes
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(c int) { s.code = c; s.ResponseWriter.WriteHeader(c) }
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: 200}
		start := time.Now()
		next.ServeHTTP(sw, r)
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.code,
			"dur", time.Since(start).Round(time.Millisecond), "remote", r.RemoteAddr)
	})
}

func main() {
	cfg := LoadConfig()
	slog.SetDefault(setupLogging(cfg))

	for _, d := range []string{cfg.DataDir, filepath.Join(cfg.CacheDir, "audio"), filepath.Join(cfg.CacheDir, "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			slog.Error("cannot create dir", "dir", d, "err", err)
			os.Exit(1)
		}
	}
	// Leftover partial transcodes from a previous crash.
	if old, _ := filepath.Glob(filepath.Join(cfg.CacheDir, "tmp", "*.part")); len(old) > 0 {
		for _, f := range old {
			os.Remove(f)
		}
	}
	for _, bin := range []string{cfg.YtDlp, cfg.FFmpeg} {
		if _, err := exec.LookPath(bin); err != nil {
			slog.Warn("required binary not found in PATH", "bin", bin)
		}
	}

	db, err := openDB(filepath.Join(cfg.DataDir, "sqlite.db"))
	if err != nil {
		slog.Error("database init failed", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	s := &Server{cfg: cfg, db: db, ytm: NewYTM(), sem: make(chan struct{}, envInt("MAX_TRANSCODES", 4))}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go s.evictLoop(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /api/v1/auth/register", s.handleRegister)
	mux.HandleFunc("PUT /api/v1/auth/cookie", s.auth(s.handleUpdateCookie))

	mux.HandleFunc("GET /api/v1/home", s.auth(s.ytmHandler(s.handleHome)))
	mux.HandleFunc("GET /api/v1/search", s.auth(s.ytmHandler(s.handleSearch)))
	mux.HandleFunc("GET /api/v1/library/playlists", s.auth(s.ytmHandler(s.handleLibrary)))
	mux.HandleFunc("GET /api/v1/ytplaylist/{id}", s.auth(s.ytmHandler(s.handleYTPlaylist)))

	mux.HandleFunc("GET /api/v1/stream/{id}", s.auth(s.handleStream))
	mux.HandleFunc("GET /api/v1/download/{id}", s.auth(s.handleDownload))

	mux.HandleFunc("GET /api/v1/playlists", s.auth(s.listPlaylists))
	mux.HandleFunc("POST /api/v1/playlists", s.auth(s.createPlaylist))
	mux.HandleFunc("GET /api/v1/playlists/{id}", s.auth(s.getPlaylist))
	mux.HandleFunc("DELETE /api/v1/playlists/{id}", s.auth(s.deletePlaylist))
	mux.HandleFunc("POST /api/v1/playlists/{id}/tracks", s.auth(s.addTrack))
	mux.HandleFunc("DELETE /api/v1/playlists/{id}/tracks/{videoId}", s.auth(s.removeTrack))

	mux.HandleFunc("POST /api/v1/history", s.auth(s.addHistory))
	mux.HandleFunc("GET /api/v1/history", s.auth(s.getHistory))
	mux.HandleFunc("GET /api/v1/preferences", s.auth(s.getPrefs))
	mux.HandleFunc("PUT /api/v1/preferences", s.auth(s.putPrefs))

	// No WriteTimeout: audio streams are long-lived.
	srv := &http.Server{Addr: ":" + cfg.Port, Handler: logRequests(mux), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		slog.Info("server listening", "addr", srv.Addr, "cache_threshold", cfg.Threshold,
			"max_cache_bytes", cfg.MaxCacheBytes, "max_tracks", cfg.MaxTracks)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	slog.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
}
