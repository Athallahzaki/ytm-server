package main

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS users(
  id INTEGER PRIMARY KEY, username TEXT UNIQUE NOT NULL, token_hash TEXT UNIQUE NOT NULL,
  yt_cookie TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS playlists(
  id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS playlist_tracks(
  playlist_id INTEGER NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
  position INTEGER NOT NULL, video_id TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '', artist TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(playlist_id, video_id));
CREATE TABLE IF NOT EXISTS history(
  id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  video_id TEXT NOT NULL, title TEXT NOT NULL DEFAULT '', artist TEXT NOT NULL DEFAULT '',
  played_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_history_user ON history(user_id, played_at DESC);
CREATE TABLE IF NOT EXISTS preferences(
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  key TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY(user_id, key));
CREATE TABLE IF NOT EXISTS tracks(
  video_id TEXT PRIMARY KEY, path TEXT NOT NULL, size INTEGER NOT NULL,
  play_count INTEGER NOT NULL DEFAULT 0, last_access INTEGER NOT NULL, cached_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS stream_counts(
  video_id TEXT PRIMARY KEY, full_streams INTEGER NOT NULL DEFAULT 0);
`

func openDB(path string) (*sql.DB, error) {
	// WAL is set via pragma in the DSN so it applies to every pooled connection.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return db, nil
}
