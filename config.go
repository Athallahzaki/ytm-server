package main

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port          string
	DataDir       string
	CacheDir      string
	Threshold     int
	MaxCacheBytes int64
	MaxTracks     int
	EvictEvery    time.Duration
	LogLevel      string
	RegKey        string
	Bitrate       string
	YtDlp         string
	FFmpeg        string
	ExtraArgs     []string
}

func env(k, d string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	if v, err := strconv.Atoi(env(k, "")); err == nil {
		return v
	}
	return d
}

func LoadConfig() Config {
	gb, err := strconv.ParseFloat(env("MAX_CACHE_GB", "20"), 64)
	if err != nil {
		gb = 20
	}
	every, err := time.ParseDuration(env("EVICT_INTERVAL", "10m"))
	if err != nil {
		every = 10 * time.Minute
	}
	return Config{
		Port:          env("PORT", "8080"),
		DataDir:       env("DATA_DIR", "./data"),
		CacheDir:      env("CACHE_DIR", "./cache"),
		Threshold:     envInt("CACHE_THRESHOLD", 3),
		MaxCacheBytes: int64(gb * (1 << 30)),
		MaxTracks:     envInt("MAX_CACHE_TRACKS", 5000),
		EvictEvery:    every,
		LogLevel:      env("LOG_LEVEL", "info"),
		RegKey:        os.Getenv("REGISTRATION_KEY"),
		Bitrate:       env("OPUS_BITRATE", "96k"),
		YtDlp:         env("YTDLP_PATH", "yt-dlp"),
		FFmpeg:        env("FFMPEG_PATH", "ffmpeg"),
		ExtraArgs:     strings.Fields(os.Getenv("YTDLP_EXTRA_ARGS")),
	}
}
