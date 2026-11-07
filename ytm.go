package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Minimal YouTube Music (InnerTube) client using the user's browser cookie,
// the same approach ytmusicapi uses. This is unofficial and may break when
// YouTube changes its responses.

const (
	ytmOrigin = "https://music.youtube.com"
	ytmAPI    = ytmOrigin + "/youtubei/v1/"
	ytmUA     = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"
	// ytmusicapi's "songs" search filter.
	songsFilter = "EgWKAQIIAWoMEA4QChADEAQQCRAF"
)

type Item struct {
	Type     string `json:"type"` // song | album | artist | playlist
	ID       string `json:"id"`
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	Thumb    string `json:"thumbnail,omitempty"`
}

type Section struct {
	Title string `json:"title"`
	Items []Item `json:"items"`
}

type YTM struct{ http *http.Client }

func NewYTM() *YTM { return &YTM{http: &http.Client{Timeout: 20 * time.Second}} }

func cookieValue(cookie, name string) string {
	for _, part := range strings.Split(cookie, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, name+"=") {
			return strings.TrimPrefix(part, name+"=")
		}
	}
	return ""
}

func sapisid(cookie string) string {
	if v := cookieValue(cookie, "__Secure-3PAPISID"); v != "" {
		return v
	}
	return cookieValue(cookie, "SAPISID")
}

func authHeader(cookie string) (string, error) {
	sid := sapisid(cookie)
	if sid == "" {
		return "", fmt.Errorf("cookie has no SAPISID / __Secure-3PAPISID")
	}
	ts := time.Now().Unix()
	sum := sha1.Sum([]byte(fmt.Sprintf("%d %s %s", ts, sid, ytmOrigin)))
	return fmt.Sprintf("SAPISIDHASH %d_%x", ts, sum), nil
}

func (y *YTM) call(ctx context.Context, cookie, endpoint string, extra map[string]any) (map[string]any, error) {
	body := map[string]any{
		"context": map[string]any{"client": map[string]any{
			"clientName": "WEB_REMIX", "clientVersion": "1.20250101.01.00", "hl": "en", "gl": "US",
		}},
	}
	for k, v := range extra {
		body[k] = v
	}
	buf, _ := json.Marshal(body)
	auth, err := authHeader(cookie)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ytmAPI+endpoint+"?prettyPrint=false", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ytmUA)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Authorization", auth)
	req.Header.Set("Origin", ytmOrigin)
	req.Header.Set("X-Origin", ytmOrigin)
	req.Header.Set("X-Goog-AuthUser", "0")
	resp, err := y.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("youtube music returned %d: %s", resp.StatusCode, snippet)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func (y *YTM) Home(ctx context.Context, cookie string) ([]Section, error) {
	return y.browse(ctx, cookie, "FEmusic_home")
}

func (y *YTM) LibraryPlaylists(ctx context.Context, cookie string) ([]Section, error) {
	return y.browse(ctx, cookie, "FEmusic_liked_playlists")
}

func (y *YTM) Playlist(ctx context.Context, cookie, id string) ([]Section, error) {
	if !strings.HasPrefix(id, "VL") && !strings.HasPrefix(id, "MPRE") && !strings.HasPrefix(id, "UC") {
		id = "VL" + id
	}
	return y.browse(ctx, cookie, id)
}

func (y *YTM) browse(ctx context.Context, cookie, browseID string) ([]Section, error) {
	res, err := y.call(ctx, cookie, "browse", map[string]any{"browseId": browseID})
	if err != nil {
		return nil, err
	}
	var out []Section
	collectSections(res, &out)
	return out, nil
}

func (y *YTM) Search(ctx context.Context, cookie, q string, songsOnly bool) ([]Section, error) {
	extra := map[string]any{"query": q}
	if songsOnly {
		extra["params"] = songsFilter
	}
	res, err := y.call(ctx, cookie, "search", extra)
	if err != nil {
		return nil, err
	}
	var out []Section
	collectSections(res, &out)
	return out, nil
}

// ---- response parsing helpers ----

func dig(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[k]
		case int:
			a, ok := v.([]any)
			if !ok || k < 0 || k >= len(a) {
				return nil
			}
			v = a[k]
		}
	}
	return v
}

func str(v any) string { s, _ := v.(string); return s }

func runsText(v any) string {
	runs, _ := dig(v, "runs").([]any)
	var sb strings.Builder
	for _, r := range runs {
		sb.WriteString(str(dig(r, "text")))
	}
	return sb.String()
}

func findThumb(v any) string {
	switch t := v.(type) {
	case map[string]any:
		if th, ok := t["thumbnails"].([]any); ok && len(th) > 0 {
			return str(dig(th[len(th)-1], "url"))
		}
		for _, c := range t {
			if u := findThumb(c); u != "" {
				return u
			}
		}
	case []any:
		for _, c := range t {
			if u := findThumb(c); u != "" {
				return u
			}
		}
	}
	return ""
}

func kindFromBrowseID(id string) string {
	switch {
	case strings.HasPrefix(id, "UC"):
		return "artist"
	case strings.HasPrefix(id, "MPRE"):
		return "album"
	case strings.HasPrefix(id, "VL"), strings.HasPrefix(id, "PL"):
		return "playlist"
	}
	return ""
}

func parseResponsive(m map[string]any) *Item {
	vid := str(dig(m, "playlistItemData", "videoId"))
	if vid == "" {
		vid = str(dig(m, "overlay", "musicItemThumbnailOverlayRenderer", "content",
			"musicPlayButtonRenderer", "playNavigationEndpoint", "watchEndpoint", "videoId"))
	}
	col := func(i int) string {
		return runsText(dig(m, "flexColumns", i, "musicResponsiveListItemFlexColumnRenderer", "text"))
	}
	it := &Item{Title: col(0), Subtitle: col(1), Thumb: findThumb(m["thumbnail"])}
	if vid != "" {
		it.Type, it.ID = "song", vid
		return it
	}
	bid := str(dig(m, "navigationEndpoint", "browseEndpoint", "browseId"))
	if k := kindFromBrowseID(bid); k != "" {
		it.Type, it.ID = k, bid
		return it
	}
	return nil
}

func parseTwoRow(m map[string]any) *Item {
	it := &Item{Title: runsText(m["title"]), Subtitle: runsText(m["subtitle"]), Thumb: findThumb(m["thumbnailRenderer"])}
	if vid := str(dig(m, "navigationEndpoint", "watchEndpoint", "videoId")); vid != "" {
		it.Type, it.ID = "song", vid
		return it
	}
	if pid := str(dig(m, "navigationEndpoint", "watchPlaylistEndpoint", "playlistId")); pid != "" {
		it.Type, it.ID = "playlist", pid
		return it
	}
	bid := str(dig(m, "navigationEndpoint", "browseEndpoint", "browseId"))
	if k := kindFromBrowseID(bid); k != "" {
		it.Type, it.ID = k, bid
		return it
	}
	return nil
}

func parseContents(v any) []Item {
	arr, _ := v.([]any)
	var items []Item
	for _, el := range arr {
		m, _ := el.(map[string]any)
		var it *Item
		if r, ok := m["musicResponsiveListItemRenderer"].(map[string]any); ok {
			it = parseResponsive(r)
		} else if r, ok := m["musicTwoRowItemRenderer"].(map[string]any); ok {
			it = parseTwoRow(r)
		}
		if it != nil && it.ID != "" {
			items = append(items, *it)
		}
	}
	return items
}

func collectSections(v any, out *[]Section) {
	switch t := v.(type) {
	case map[string]any:
		if sh, ok := t["musicCarouselShelfRenderer"].(map[string]any); ok {
			title := runsText(dig(sh, "header", "musicCarouselShelfBasicHeaderRenderer", "title"))
			if items := parseContents(sh["contents"]); len(items) > 0 {
				*out = append(*out, Section{Title: title, Items: items})
			}
			return
		}
		for _, key := range []string{"musicShelfRenderer", "musicPlaylistShelfRenderer"} {
			if sh, ok := t[key].(map[string]any); ok {
				title := runsText(sh["title"])
				if title == "" && key == "musicPlaylistShelfRenderer" {
					title = "Tracks"
				}
				if items := parseContents(sh["contents"]); len(items) > 0 {
					*out = append(*out, Section{Title: title, Items: items})
				}
				return
			}
		}
		if g, ok := t["gridRenderer"].(map[string]any); ok {
			if items := parseContents(g["items"]); len(items) > 0 {
				*out = append(*out, Section{Title: "Library", Items: items})
			}
			return
		}
		for _, c := range t {
			collectSections(c, out)
		}
	case []any:
		for _, c := range t {
			collectSections(c, out)
		}
	}
}
