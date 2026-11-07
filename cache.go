package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func (s *Server) audioPath(id string) string {
	return filepath.Join(s.cfg.CacheDir, "audio", id+".opus")
}

// cachedPath returns the cached file for a track, dropping stale DB rows.
func (s *Server) cachedPath(id string) (string, bool) {
	var p string
	if err := s.db.QueryRow(`SELECT path FROM tracks WHERE video_id=?`, id).Scan(&p); err != nil {
		return "", false
	}
	if _, err := os.Stat(p); err != nil {
		s.db.Exec(`DELETE FROM tracks WHERE video_id=?`, id)
		return "", false
	}
	return p, true
}

func (s *Server) touchTrack(id string) {
	s.db.Exec(`UPDATE tracks SET play_count=play_count+1, last_access=? WHERE video_id=?`, time.Now().Unix(), id)
}

// recordFullStream is called after a track has been fully delivered. It bumps
// the full-stream counter and promotes the temp file into the permanent cache
// once the threshold is reached. The temp file is always consumed.
func (s *Server) recordFullStream(id, tmp string) {
	var n int
	err := s.db.QueryRow(`INSERT INTO stream_counts(video_id, full_streams) VALUES(?,1)
		ON CONFLICT(video_id) DO UPDATE SET full_streams=full_streams+1 RETURNING full_streams`, id).Scan(&n)
	if err != nil {
		slog.Error("stream count failed", "id", id, "err", err)
		os.Remove(tmp)
		return
	}
	if _, cached := s.cachedPath(id); cached || n < s.cfg.Threshold {
		slog.Debug("not caching", "id", id, "full_streams", n, "threshold", s.cfg.Threshold)
		os.Remove(tmp)
		return
	}
	dst := s.audioPath(id)
	if err := os.Rename(tmp, dst); err != nil {
		slog.Error("cache commit failed", "id", id, "err", err)
		os.Remove(tmp)
		return
	}
	st, _ := os.Stat(dst)
	now := time.Now().Unix()
	_, err = s.db.Exec(`INSERT OR REPLACE INTO tracks(video_id,path,size,play_count,last_access,cached_at) VALUES(?,?,?,?,?,?)`,
		id, dst, st.Size(), n, now, now)
	if err != nil {
		slog.Error("cache index failed", "id", id, "err", err)
		os.Remove(dst)
		return
	}
	slog.Info("track cached", "id", id, "bytes", st.Size(), "full_streams", n)
}

func (s *Server) evictLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.EvictEvery)
	defer t.Stop()
	for {
		s.evictOnce()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// evictOnce removes the lowest-scoring tracks until the cache is under 90% of
// its limits. Score = play_count / (1 + days since last access): frequently
// played (LFU) and recently played (LRU) tracks both score high.
func (s *Server) evictOnce() {
	rows, err := s.db.Query(`SELECT video_id, path, size, play_count, last_access FROM tracks`)
	if err != nil {
		slog.Error("evict query failed", "err", err)
		return
	}
	type row struct {
		id, path string
		size     int64
		score    float64
	}
	var all []row
	var total int64
	now := time.Now().Unix()
	for rows.Next() {
		var r row
		var plays, last int64
		if rows.Scan(&r.id, &r.path, &r.size, &plays, &last) != nil {
			continue
		}
		age := float64(now-last) / 86400
		r.score = float64(plays) / (1 + age)
		total += r.size
		all = append(all, r)
	}
	rows.Close()

	if total <= s.cfg.MaxCacheBytes && len(all) <= s.cfg.MaxTracks {
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].score < all[j].score })
	targetBytes := int64(float64(s.cfg.MaxCacheBytes) * 0.9)
	targetCount := int(float64(s.cfg.MaxTracks) * 0.9)
	evicted := 0
	for _, r := range all {
		if total <= targetBytes && len(all)-evicted <= targetCount {
			break
		}
		os.Remove(r.path)
		s.db.Exec(`DELETE FROM tracks WHERE video_id=?`, r.id)
		total -= r.size
		evicted++
	}
	slog.Info("cache eviction", "evicted", evicted, "remaining_bytes", total)
}
