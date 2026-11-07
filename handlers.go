package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type ctxKey struct{}

type User struct {
	ID     int64
	Name   string
	Cookie string
}

var (
	usernameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{2,32}$`)
	browseIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{2,80}$`)
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		jsonErr(w, 400, "invalid JSON body")
		return false
	}
	return true
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func userFrom(r *http.Request) User { u, _ := r.Context().Value(ctxKey{}).(User); return u }

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" {
			jsonErr(w, 401, "missing bearer token")
			return
		}
		var u User
		err := s.db.QueryRow(`SELECT id, username, yt_cookie FROM users WHERE token_hash=?`, hashToken(tok)).
			Scan(&u.ID, &u.Name, &u.Cookie)
		if err != nil {
			jsonErr(w, 401, "invalid token")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
	}
}

func validCookie(c string) bool {
	return !strings.ContainsAny(c, "\r\n") && sapisid(c) != ""
}

// ---- auth ----

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Cookie string }
	if !readJSON(w, r, &in) {
		return
	}
	if s.cfg.RegKey != "" &&
		subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Registration-Key")), []byte(s.cfg.RegKey)) != 1 {
		jsonErr(w, 403, "bad registration key")
		return
	}
	if !usernameRe.MatchString(in.Username) {
		jsonErr(w, 400, "username must be 2-32 chars: letters, digits, _ . -")
		return
	}
	if !validCookie(in.Cookie) {
		jsonErr(w, 400, "cookie must be the full Cookie header from music.youtube.com (needs SAPISID)")
		return
	}
	b := make([]byte, 32)
	rand.Read(b)
	token := "ytm_" + hex.EncodeToString(b)
	_, err := s.db.Exec(`INSERT INTO users(username, token_hash, yt_cookie, created_at) VALUES(?,?,?,?)`,
		in.Username, hashToken(token), in.Cookie, time.Now().Unix())
	if err != nil {
		jsonErr(w, 409, "username already exists")
		return
	}
	slog.Info("user registered", "user", in.Username)
	writeJSON(w, 201, map[string]string{"token": token})
}

func (s *Server) handleUpdateCookie(w http.ResponseWriter, r *http.Request) {
	var in struct{ Cookie string }
	if !readJSON(w, r, &in) {
		return
	}
	if !validCookie(in.Cookie) {
		jsonErr(w, 400, "invalid cookie")
		return
	}
	s.db.Exec(`UPDATE users SET yt_cookie=? WHERE id=?`, in.Cookie, userFrom(r).ID)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- YouTube Music proxy ----

func (s *Server) ytmHandler(f func(r *http.Request, u User) ([]Section, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secs, err := f(r, userFrom(r))
		if err != nil {
			slog.Error("youtube music request failed", "path", r.URL.Path, "err", err)
			jsonErr(w, 502, "youtube music request failed (cookie may have expired)")
			return
		}
		writeJSON(w, 200, map[string]any{"sections": secs})
	}
}

func (s *Server) handleHome(r *http.Request, u User) ([]Section, error) {
	return s.ytm.Home(r.Context(), modeOf(r), u.Cookie)
}

func (s *Server) handleSearch(r *http.Request, u User) ([]Section, error) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		return nil, nil
	}
	return s.ytm.Search(r.Context(), modeOf(r), u.Cookie, q, r.URL.Query().Get("filter") != "")
}

func (s *Server) handleLibrary(r *http.Request, u User) ([]Section, error) {
	return s.ytm.LibraryPlaylists(r.Context(), modeOf(r), u.Cookie)
}

func (s *Server) handleYTPlaylist(r *http.Request, u User) ([]Section, error) {
	id := r.PathValue("id")
	if !browseIDRe.MatchString(id) {
		return nil, context.Canceled
	}
	return s.ytm.Playlist(r.Context(), modeOf(r), u.Cookie, id)
}

// ---- local playlists ----

func (s *Server) ownsPlaylist(r *http.Request) (int64, bool) {
	pid, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, false
	}
	var one int
	err = s.db.QueryRow(`SELECT 1 FROM playlists WHERE id=? AND user_id=?`, pid, userFrom(r).ID).Scan(&one)
	return pid, err == nil
}

func (s *Server) listPlaylists(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT p.id, p.name, (SELECT COUNT(*) FROM playlist_tracks WHERE playlist_id=p.id)
		FROM playlists p WHERE user_id=? ORDER BY p.created_at DESC`, userFrom(r).ID)
	if err != nil {
		jsonErr(w, 500, "db error")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id int64
		var name string
		var n int
		rows.Scan(&id, &name, &n)
		out = append(out, map[string]any{"id": id, "name": name, "trackCount": n})
	}
	writeJSON(w, 200, out)
}

func (s *Server) createPlaylist(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name   string    `json:"name"`
		Tracks []trackIn `json:"tracks"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 100 {
		jsonErr(w, 400, "name required (max 100 chars)")
		return
	}
	if len(in.Tracks) > 1000 {
		jsonErr(w, 400, "too many tracks (max 1000)")
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		jsonErr(w, 500, "db error")
		return
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO playlists(user_id,name,created_at) VALUES(?,?,?)`,
		userFrom(r).ID, in.Name, time.Now().Unix())
	if err != nil {
		jsonErr(w, 500, "db error")
		return
	}
	id, _ := res.LastInsertId()
	pos := 0
	for _, t := range in.Tracks {
		if !videoIDRe.MatchString(t.VideoID) {
			continue
		}
		if rr, err := tx.Exec(`INSERT OR IGNORE INTO playlist_tracks(playlist_id, position, video_id, title, artist) VALUES(?,?,?,?,?)`,
			id, pos, t.VideoID, t.Title, t.Artist); err == nil {
			if n, _ := rr.RowsAffected(); n > 0 {
				pos++
			}
		}
	}
	if err := tx.Commit(); err != nil {
		jsonErr(w, 500, "db error")
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "name": in.Name, "trackCount": pos})
}

func (s *Server) getPlaylist(w http.ResponseWriter, r *http.Request) {
	pid, ok := s.ownsPlaylist(r)
	if !ok {
		jsonErr(w, 404, "playlist not found")
		return
	}
	var name string
	s.db.QueryRow(`SELECT name FROM playlists WHERE id=?`, pid).Scan(&name)
	rows, _ := s.db.Query(`SELECT video_id, title, artist FROM playlist_tracks WHERE playlist_id=? ORDER BY position`, pid)
	defer rows.Close()
	tracks := []map[string]string{}
	for rows.Next() {
		var v, t, a string
		rows.Scan(&v, &t, &a)
		tracks = append(tracks, map[string]string{"videoId": v, "title": t, "artist": a})
	}
	writeJSON(w, 200, map[string]any{"id": pid, "name": name, "tracks": tracks})
}

func (s *Server) deletePlaylist(w http.ResponseWriter, r *http.Request) {
	pid, ok := s.ownsPlaylist(r)
	if !ok {
		jsonErr(w, 404, "playlist not found")
		return
	}
	s.db.Exec(`DELETE FROM playlists WHERE id=?`, pid)
	w.WriteHeader(204)
}

type trackIn struct {
	VideoID string `json:"videoId"`
	Title   string `json:"title"`
	Artist  string `json:"artist"`
}

func (s *Server) addTrack(w http.ResponseWriter, r *http.Request) {
	pid, ok := s.ownsPlaylist(r)
	if !ok {
		jsonErr(w, 404, "playlist not found")
		return
	}
	var in trackIn
	if !readJSON(w, r, &in) {
		return
	}
	if !videoIDRe.MatchString(in.VideoID) {
		jsonErr(w, 400, "invalid videoId")
		return
	}
	_, err := s.db.Exec(`INSERT OR IGNORE INTO playlist_tracks(playlist_id, position, video_id, title, artist)
		VALUES(?, (SELECT COALESCE(MAX(position),-1)+1 FROM playlist_tracks WHERE playlist_id=?), ?, ?, ?)`,
		pid, pid, in.VideoID, in.Title, in.Artist)
	if err != nil {
		jsonErr(w, 500, "db error")
		return
	}
	writeJSON(w, 201, map[string]bool{"ok": true})
}

func (s *Server) removeTrack(w http.ResponseWriter, r *http.Request) {
	pid, ok := s.ownsPlaylist(r)
	if !ok {
		jsonErr(w, 404, "playlist not found")
		return
	}
	s.db.Exec(`DELETE FROM playlist_tracks WHERE playlist_id=? AND video_id=?`, pid, r.PathValue("videoId"))
	w.WriteHeader(204)
}

// ---- history & preferences ----

func (s *Server) addHistory(w http.ResponseWriter, r *http.Request) {
	var in trackIn
	if !readJSON(w, r, &in) {
		return
	}
	if !videoIDRe.MatchString(in.VideoID) {
		jsonErr(w, 400, "invalid videoId")
		return
	}
	s.db.Exec(`INSERT INTO history(user_id, video_id, title, artist, played_at) VALUES(?,?,?,?,?)`,
		userFrom(r).ID, in.VideoID, in.Title, in.Artist, time.Now().Unix())
	writeJSON(w, 201, map[string]bool{"ok": true})
}

func (s *Server) getHistory(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT video_id, title, artist, played_at FROM history
		WHERE user_id=? ORDER BY played_at DESC, id DESC LIMIT ?`, userFrom(r).ID, limit)
	if err != nil {
		jsonErr(w, 500, "db error")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var v, t, a string
		var at int64
		rows.Scan(&v, &t, &a, &at)
		out = append(out, map[string]any{"videoId": v, "title": t, "artist": a, "playedAt": at})
	}
	writeJSON(w, 200, out)
}

func (s *Server) getPrefs(w http.ResponseWriter, r *http.Request) {
	rows, _ := s.db.Query(`SELECT key, value FROM preferences WHERE user_id=?`, userFrom(r).ID)
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		rows.Scan(&k, &v)
		out[k] = v
	}
	writeJSON(w, 200, out)
}

func (s *Server) putPrefs(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	if !readJSON(w, r, &in) {
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		jsonErr(w, 500, "db error")
		return
	}
	for k, v := range in {
		tx.Exec(`INSERT INTO preferences(user_id,key,value) VALUES(?,?,?)
			ON CONFLICT(user_id,key) DO UPDATE SET value=excluded.value`, userFrom(r).ID, k, v)
	}
	tx.Commit()
	writeJSON(w, 200, map[string]bool{"ok": true})
}
