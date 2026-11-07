package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func modeOf(r *http.Request) Mode { return parseMode(r.URL.Query().Get("mode")) }

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------- seekable files
// Instead of piping a live transcode (no length, no seeking), we finish
// transcoding to a file first and serve it with Content-Length + Range support.

type job struct {
	done chan struct{}
	path string
	err  error
}

func (s *Server) warmPath(id string) string { return filepath.Join(s.cfg.CacheDir, "warm", id+".opus") }

// ensureFile returns a complete Opus file for the track (cached, warm or freshly
// transcoded). Concurrent callers share one transcode.
func (s *Server) ensureFile(ctx context.Context, id string) (string, error) {
	if p, ok := s.cachedPath(id); ok {
		return p, nil
	}
	warm := s.warmPath(id)
	if st, err := os.Stat(warm); err == nil && st.Size() > 0 {
		now := time.Now()
		os.Chtimes(warm, now, now)
		return warm, nil
	}
	s.jobsMu.Lock()
	j, running := s.jobs[id]
	if !running {
		j = &job{done: make(chan struct{})}
		s.jobs[id] = j
		go s.runJob(id, warm, j)
	}
	s.jobsMu.Unlock()
	select {
	case <-j.done:
		return j.path, j.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *Server) runJob(id, dst string, j *job) {
	defer func() {
		close(j.done)
		s.jobsMu.Lock()
		delete(s.jobs, id)
		s.jobsMu.Unlock()
	}()
	// Detached context: one impatient client must not kill a transcode others wait on.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	start := time.Now()
	p, err := s.startPipeline(ctx, id)
	if err != nil {
		j.err = err
		return
	}
	tmp, err := os.CreateTemp(s.tmpDir(), id+"-*.part")
	if err != nil {
		p.cancel()
		p.wait()
		j.err = err
		return
	}
	_, cerr := io.Copy(tmp, p.out)
	if cerr != nil {
		p.cancel()
	}
	werr := p.wait()
	tmp.Close()
	if cerr != nil || werr != nil {
		os.Remove(tmp.Name())
		j.err = fmt.Errorf("transcode failed: copy=%v wait=%v", cerr, werr)
		return
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		os.Remove(tmp.Name())
		j.err = err
		return
	}
	j.path = dst
	slog.Info("transcoded", "id", id, "dur", time.Since(start).Round(time.Millisecond))
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, id string) {
	path, err := s.ensureFile(r.Context(), id)
	if err != nil {
		slog.Error("could not prepare audio", "id", id, "err", err)
		jsonErr(w, 502, "could not fetch audio")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		jsonErr(w, 500, "file error")
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	if rg := r.Header.Get("Range"); rg == "" || strings.HasPrefix(rg, "bytes=0-") {
		s.db.Exec(`UPDATE tracks SET last_access=? WHERE video_id=?`, time.Now().Unix(), id)
	}
	w.Header().Set("Content-Type", "audio/ogg")
	http.ServeContent(w, r, id+".opus", st.ModTime(), f)
}

// GET /stream/{id}: seekable file (default). ?live=1 gives the old instant-start pipe.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !videoIDRe.MatchString(id) {
		jsonErr(w, 400, "invalid video id")
		return
	}
	if r.URL.Query().Get("live") == "1" {
		s.handleLive(w, r)
		return
	}
	s.serveFile(w, r, id)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !videoIDRe.MatchString(id) {
		jsonErr(w, 400, "invalid video id")
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.opus"`, id))
	s.serveFile(w, r, id)
}

// POST /prepare/{id}: warm the next track so skipping is instant.
func (s *Server) handlePrepare(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !videoIDRe.MatchString(id) {
		jsonErr(w, 400, "invalid video id")
		return
	}
	go s.ensureFile(context.Background(), id)
	w.WriteHeader(http.StatusAccepted)
}

// POST /played/{id}: the client reports a (mostly) completed listen. This drives
// the cache threshold now that files are fetched ahead of playback.
func (s *Server) handlePlayed(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !videoIDRe.MatchString(id) {
		jsonErr(w, 400, "invalid video id")
		return
	}
	n, err := s.countPlay(id)
	if err != nil {
		jsonErr(w, 500, "db error")
		return
	}
	if _, ok := s.cachedPath(id); ok {
		s.db.Exec(`UPDATE tracks SET play_count=play_count+1, last_access=? WHERE video_id=?`, time.Now().Unix(), id)
	} else if n >= s.cfg.Threshold {
		go func() {
			p, err := s.ensureFile(context.Background(), id)
			if err != nil {
				return
			}
			if _, cached := s.cachedPath(id); !cached && filepath.Dir(p) == filepath.Dir(s.warmPath(id)) {
				s.commitToCache(id, p, n)
			}
		}()
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- queue / playlists

func (s *Server) handleQueue(r *http.Request, u User) ([]Section, error) {
	q := r.URL.Query()
	vid, pl := q.Get("videoId"), q.Get("playlistId")
	if (vid == "" && pl == "") || (vid != "" && !videoIDRe.MatchString(vid)) || (pl != "" && !browseIDRe.MatchString(pl)) {
		return nil, fmt.Errorf("videoId or playlistId required")
	}
	return s.ytm.Queue(r.Context(), modeOf(r), u.Cookie, vid, pl)
}

func (s *Server) setOrder(w http.ResponseWriter, r *http.Request) {
	pid, ok := s.ownsPlaylist(r)
	if !ok {
		jsonErr(w, 404, "playlist not found")
		return
	}
	var in struct {
		VideoIDs []string `json:"videoIds"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		jsonErr(w, 500, "db error")
		return
	}
	defer tx.Rollback()
	for i, v := range in.VideoIDs {
		tx.Exec(`UPDATE playlist_tracks SET position=? WHERE playlist_id=? AND video_id=?`, i, pid, v)
	}
	tx.Commit()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// POST /ytplaylists: creates a real playlist on the user's YouTube account.
func (s *Server) handleCreateYT(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title    string   `json:"title"`
		Privacy  string   `json:"privacy"`
		VideoIDs []string `json:"videoIds"`
		Mode     string   `json:"mode"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" || len(in.Title) > 100 {
		jsonErr(w, 400, "title required (max 100 chars)")
		return
	}
	switch in.Privacy {
	case "PUBLIC", "UNLISTED", "PRIVATE":
	default:
		in.Privacy = "PRIVATE"
	}
	if len(in.VideoIDs) > 200 {
		jsonErr(w, 400, "too many tracks (max 200)")
		return
	}
	for _, v := range in.VideoIDs {
		if !videoIDRe.MatchString(v) {
			jsonErr(w, 400, "invalid videoId in list")
			return
		}
	}
	id, err := s.ytm.CreatePlaylist(r.Context(), parseMode(in.Mode), userFrom(r).Cookie, in.Title, in.Privacy, in.VideoIDs)
	if err != nil {
		slog.Error("create youtube playlist failed", "err", err)
		jsonErr(w, 502, "could not create the YouTube playlist")
		return
	}
	writeJSON(w, 201, map[string]string{"playlistId": id})
}

// ---------------------------------------------------------------- computer pairing
// Phone: POST /api/v1/pair/start -> shows code. Computer: open /pair, paste the
// cookie + code. Phone polls /api/v1/pair/status and receives its token.

type pairing struct {
	username, secret, cookie string
	reauthUser               int64
	created                  time.Time
}

func (s *Server) pairCleanup() { // caller holds pairMu
	for c, p := range s.pairs {
		if time.Since(p.created) > 10*time.Minute {
			delete(s.pairs, c)
		}
	}
}

func (s *Server) tokenUser(r *http.Request) (User, bool) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		return User{}, false
	}
	var u User
	err := s.db.QueryRow(`SELECT id, username, yt_cookie FROM users WHERE token_hash=?`, hashToken(tok)).Scan(&u.ID, &u.Name, &u.Cookie)
	return u, err == nil
}

func (s *Server) createUser(username, cookie string) (string, error) {
	token := "ytm_" + randHex(32)
	_, err := s.db.Exec(`INSERT INTO users(username, token_hash, yt_cookie, created_at) VALUES(?,?,?,?)`,
		username, hashToken(token), cookie, time.Now().Unix())
	return token, err
}

func (s *Server) handlePairStart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	var reauth int64
	if u, ok := s.tokenUser(r); ok {
		reauth, in.Username = u.ID, u.Name
	} else {
		if s.cfg.RegKey != "" &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Registration-Key")), []byte(s.cfg.RegKey)) != 1 {
			jsonErr(w, 403, "bad registration key")
			return
		}
		if !usernameRe.MatchString(in.Username) {
			jsonErr(w, 400, "username must be 2-32 chars: letters, digits, _ . -")
			return
		}
		var one int
		if s.db.QueryRow(`SELECT 1 FROM users WHERE username=?`, in.Username).Scan(&one) == nil {
			jsonErr(w, 409, "username already exists")
			return
		}
	}
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	s.pairCleanup()
	if len(s.pairs) >= 20 {
		jsonErr(w, 429, "too many pairing requests, try again later")
		return
	}
	var code string
	for {
		n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
		code = fmt.Sprintf("%06d", n.Int64())
		if _, taken := s.pairs[code]; !taken {
			break
		}
	}
	secret := randHex(16)
	s.pairs[code] = &pairing{username: in.Username, secret: secret, reauthUser: reauth, created: time.Now()}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	writeJSON(w, 200, map[string]any{"code": code, "secret": secret, "url": scheme + "://" + r.Host + "/pair", "expiresIn": 600})
}

func (s *Server) handlePairSubmit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code   string `json:"code"`
		Cookie string `json:"cookie"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	code := strings.Map(func(c rune) rune {
		if c >= '0' && c <= '9' {
			return c
		}
		return -1
	}, in.Code)
	cookie := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(in.Cookie), "Cookie:"))
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	s.pairCleanup()
	if time.Since(s.pairFailAt) > 10*time.Minute {
		s.pairFails = 0
	}
	if s.pairFails >= 10 {
		jsonErr(w, 429, "too many wrong codes, wait a few minutes")
		return
	}
	p, ok := s.pairs[code]
	if !ok {
		s.pairFails++
		s.pairFailAt = time.Now()
		jsonErr(w, 404, "unknown or expired code")
		return
	}
	if !validCookie(cookie) {
		jsonErr(w, 400, "that doesn't look like the Cookie header (no SAPISID found)")
		return
	}
	p.cookie = cookie
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handlePairStatus(w http.ResponseWriter, r *http.Request) {
	code, secret := r.URL.Query().Get("code"), r.URL.Query().Get("secret")
	s.pairMu.Lock()
	p, ok := s.pairs[code]
	if !ok || subtle.ConstantTimeCompare([]byte(secret), []byte(p.secret)) != 1 {
		s.pairMu.Unlock()
		jsonErr(w, 404, "unknown or expired code")
		return
	}
	if p.cookie == "" {
		s.pairMu.Unlock()
		writeJSON(w, 200, map[string]string{"status": "pending"})
		return
	}
	delete(s.pairs, code)
	s.pairMu.Unlock()

	if p.reauthUser != 0 {
		s.db.Exec(`UPDATE users SET yt_cookie=? WHERE id=?`, p.cookie, p.reauthUser)
		writeJSON(w, 200, map[string]string{"status": "done"})
		return
	}
	token, err := s.createUser(p.username, p.cookie)
	if err != nil {
		jsonErr(w, 409, "username already exists")
		return
	}
	slog.Info("user registered via pairing", "user", p.username)
	writeJSON(w, 200, map[string]string{"status": "done", "token": token})
}

const pairPage = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Pair your phone</title>
<style>body{font:16px system-ui;max-width:640px;margin:2rem auto;padding:0 1rem;line-height:1.5}
textarea,input{width:100%;box-sizing:border-box;font:inherit;padding:.6rem;margin:.3rem 0 1rem}
textarea{height:9rem;font-family:monospace;font-size:13px}button{font:inherit;padding:.7rem 1.4rem}
#msg{margin-top:1rem;font-weight:600}</style>
<h1>Pair your phone</h1>
<ol><li>Open <b>music.youtube.com</b> in this browser, signed in.</li>
<li>Press F12 &rarr; Network tab, reload, click a request to <code>music.youtube.com</code>,
copy the whole <b>Cookie</b> request header.</li>
<li>Paste it below with the 6-digit code shown on your phone.</li></ol>
<label>Code from phone</label><input id="code" inputmode="numeric" autocomplete="off" placeholder="123456">
<label>Cookie header</label><textarea id="cookie" placeholder="SID=...; SAPISID=...; ..."></textarea>
<button id="go">Send to phone</button><div id="msg"></div>
<script>
document.getElementById('go').onclick=async function(){
 var m=document.getElementById('msg');m.textContent='Sending...';
 try{
  var r=await fetch('/pair/submit',{method:'POST',headers:{'Content-Type':'application/json'},
   body:JSON.stringify({code:document.getElementById('code').value,cookie:document.getElementById('cookie').value})});
  var j=await r.json();
  m.textContent=r.ok?'Done. Your phone will sign in automatically. You can close this tab.':(j.error||'Failed');
  if(r.ok)document.getElementById('cookie').value='';
 }catch(e){m.textContent='Could not reach the server.';}
};
</script>`

func (s *Server) handlePairPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, pairPage)
}
