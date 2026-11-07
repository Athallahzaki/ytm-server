package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var videoIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

type limitedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *limitedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.b.Len() < 4096 {
		l.b.Write(p)
	}
	return len(p), nil
}

func (l *limitedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.TrimSpace(l.b.String())
}

// pipeline: yt-dlp (best audio, no video) -> ffmpeg (Opus in Ogg) -> out.
type pipeline struct {
	out    *bufio.Reader
	cancel context.CancelFunc
	wait   func() error
}

func (s *Server) startPipeline(ctx context.Context, id string) (*pipeline, error) {
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var once sync.Once
	release := func() { once.Do(func() { <-s.sem }) }

	cctx, cancel := context.WithCancel(ctx)
	fail := func(err error) (*pipeline, error) { cancel(); release(); return nil, err }

	args := append([]string{}, s.cfg.ExtraArgs...)
	args = append(args, "-f", "bestaudio/best", "--no-playlist", "--no-warnings", "-q", "-o", "-",
		"https://music.youtube.com/watch?v="+id)
	dl := exec.CommandContext(cctx, s.cfg.YtDlp, args...)
	dlErr := &limitedBuf{}
	dl.Stderr = dlErr
	dlOut, err := dl.StdoutPipe()
	if err != nil {
		return fail(err)
	}

	ff := exec.CommandContext(cctx, s.cfg.FFmpeg, "-hide_banner", "-loglevel", "error",
		"-i", "pipe:0", "-vn", "-map", "0:a:0", "-c:a", "libopus",
		"-b:a", s.cfg.Bitrate, "-vbr", "on", "-f", "ogg", "pipe:1")
	ffErr := &limitedBuf{}
	ff.Stderr = ffErr
	ff.Stdin = dlOut
	ffOut, err := ff.StdoutPipe()
	if err != nil {
		return fail(err)
	}

	if err := dl.Start(); err != nil {
		return fail(fmt.Errorf("start yt-dlp: %w", err))
	}
	if err := ff.Start(); err != nil {
		cancel()
		dl.Wait()
		release()
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	p := &pipeline{out: bufio.NewReaderSize(ffOut, 64<<10), cancel: cancel}
	p.wait = func() error {
		defer release()
		defer cancel()
		e1 := ff.Wait()
		e2 := dl.Wait()
		if e1 != nil {
			return fmt.Errorf("ffmpeg: %v: %s", e1, ffErr.String())
		}
		if e2 != nil {
			return fmt.Errorf("yt-dlp: %v: %s", e2, dlErr.String())
		}
		return nil
	}

	// Make sure we actually get audio before committing to a 200 response.
	if _, err := p.out.Peek(1); err != nil {
		cancel()
		werr := p.wait()
		return nil, fmt.Errorf("no audio produced: %v", werr)
	}
	return p, nil
}

type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

// softWriter never fails the stream if the side copy (temp file) breaks.
type softWriter struct {
	f   *os.File
	bad bool
}

func (sw *softWriter) Write(p []byte) (int, error) {
	if !sw.bad {
		if _, err := sw.f.Write(p); err != nil {
			sw.bad = true
			slog.Warn("temp write failed, track will not be cached", "err", err)
		}
	}
	return len(p), nil
}

func (s *Server) tmpDir() string { return filepath.Join(s.cfg.CacheDir, "tmp") }

func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !videoIDRe.MatchString(id) {
		jsonErr(w, 400, "invalid video id")
		return
	}
	w.Header().Set("Content-Type", "audio/ogg")
	if p, ok := s.cachedPath(id); ok {
		s.touchTrack(id)
		slog.Debug("serving from cache", "id", id)
		http.ServeFile(w, r, p) // supports Range requests for seeking
		return
	}

	start := time.Now()
	p, err := s.startPipeline(r.Context(), id)
	if err != nil {
		slog.Error("pipeline failed", "id", id, "err", err)
		jsonErr(w, 502, "could not fetch audio")
		return
	}
	defer p.cancel()

	var dst io.Writer
	var soft *softWriter
	tmp, terr := os.CreateTemp(s.tmpDir(), id+"-*.part")
	fl, _ := w.(http.Flusher)
	if terr == nil {
		soft = &softWriter{f: tmp}
		dst = io.MultiWriter(flushWriter{w, fl}, soft)
	} else {
		dst = flushWriter{w, fl}
	}

	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK) // no Content-Length -> chunked transfer encoding
	n, cerr := io.Copy(dst, p.out)
	if cerr != nil {
		p.cancel()
	}
	werr := p.wait()
	ok := cerr == nil && werr == nil && r.Context().Err() == nil
	slog.Info("stream finished", "id", id, "bytes", n, "complete", ok, "dur", time.Since(start).Round(time.Millisecond))

	if tmp != nil {
		tmp.Close()
		if ok && !soft.bad {
			s.recordFullStream(id, tmp.Name())
		} else {
			os.Remove(tmp.Name())
		}
	}
}
