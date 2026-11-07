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

// Mode selects which InnerTube client/site is used. Both share one backend and
// one set of cookies; they differ only in client identity and response layout.
type Mode string

const (
	ModeMusic   Mode = "music"
	ModeYouTube Mode = "youtube"
)

const (
	ytmUA        = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"
	songsFilter  = "EgWKAQIIAWoMEA4QChADEAQQCRAF"
	videosFilter = "EgIQAQ%3D%3D"
	// Public key embedded in YouTube's own web client (not a secret).
	ytWebKey = "AIzaSyAO_FJ2SlqU8Q4STEHLGCilw_Y9_11qcW8"
)

func parseMode(s string) Mode {
	if s == string(ModeYouTube) {
		return ModeYouTube
	}
	return ModeMusic
}

type site struct{ origin, client, version, clientID string }

func (m Mode) site() site {
	if m == ModeYouTube {
		return site{"https://www.youtube.com", "WEB", "2.20250101.00.00", "1"}
	}
	return site{"https://music.youtube.com", "WEB_REMIX", "1.20250101.01.00", "67"}
}

type Item struct {
	Type     string `json:"type"` // song | album | artist | playlist | mix
	ID       string `json:"id"`
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	Thumb    string `json:"thumbnail,omitempty"`
	Duration int    `json:"duration,omitempty"` // seconds, 0 = unknown
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

func authHeader(cookie, origin string) (string, error) {
	sid := sapisid(cookie)
	if sid == "" {
		return "", fmt.Errorf("cookie has no SAPISID / __Secure-3PAPISID")
	}
	ts := time.Now().Unix()
	sum := sha1.Sum([]byte(fmt.Sprintf("%d %s %s", ts, sid, origin)))
	return fmt.Sprintf("SAPISIDHASH %d_%x", ts, sum), nil
}

func (y *YTM) call(ctx context.Context, mode Mode, cookie, endpoint string, extra map[string]any) (map[string]any, error) {
	st := mode.site()
	body := map[string]any{
		"context": map[string]any{"client": map[string]any{
			"clientName": st.client, "clientVersion": st.version, "hl": "en", "gl": "US",
		}},
	}
	for k, v := range extra {
		body[k] = v
	}
	buf, _ := json.Marshal(body)
	auth, err := authHeader(cookie, st.origin)
	if err != nil {
		return nil, err
	}
	url := st.origin + "/youtubei/v1/" + endpoint + "?prettyPrint=false"
	if mode == ModeYouTube {
		url += "&key=" + ytWebKey
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ytmUA)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Authorization", auth)
	req.Header.Set("Origin", st.origin)
	req.Header.Set("X-Origin", st.origin)
	req.Header.Set("X-Goog-AuthUser", "0")
	req.Header.Set("X-YouTube-Client-Name", st.clientID)
	req.Header.Set("X-YouTube-Client-Version", st.version)
	resp, err := y.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("youtube returned %d: %s", resp.StatusCode, snippet)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func (y *YTM) parse(mode Mode, res map[string]any, videoTitle string, mixesFirst bool) []Section {
	if mode == ModeYouTube {
		var c ytCollected
		walkYT(res, &c)
		return c.sections(videoTitle, mixesFirst)
	}
	var out []Section
	collectSections(res, &out)
	return out
}

func (y *YTM) browse(ctx context.Context, mode Mode, cookie, browseID, videoTitle string, mixesFirst bool) ([]Section, error) {
	res, err := y.call(ctx, mode, cookie, "browse", map[string]any{"browseId": browseID})
	if err != nil {
		return nil, err
	}
	return y.parse(mode, res, videoTitle, mixesFirst), nil
}

func (y *YTM) Home(ctx context.Context, mode Mode, cookie string) ([]Section, error) {
	if mode == ModeYouTube {
		return y.browse(ctx, mode, cookie, "FEwhat_to_watch", "Recommended", true)
	}
	return y.browse(ctx, mode, cookie, "FEmusic_home", "", false)
}

func (y *YTM) LibraryPlaylists(ctx context.Context, mode Mode, cookie string) ([]Section, error) {
	if mode == ModeYouTube {
		return y.browse(ctx, mode, cookie, "FEplaylist_aggregation", "Videos", false)
	}
	return y.browse(ctx, mode, cookie, "FEmusic_liked_playlists", "", false)
}

// Playlist opens a playlist/album/channel. Mixes (RD...) are "radio" playlists
// and only exist through the `next` endpoint, so they are routed to Queue.
func (y *YTM) Playlist(ctx context.Context, mode Mode, cookie, id string) ([]Section, error) {
	if isRadioID(id) {
		return y.Queue(ctx, mode, cookie, "", id)
	}
	if !strings.HasPrefix(id, "VL") && !strings.HasPrefix(id, "MPRE") && !strings.HasPrefix(id, "UC") {
		id = "VL" + id
	}
	return y.browse(ctx, mode, cookie, id, "Tracks", false)
}

func (y *YTM) Search(ctx context.Context, mode Mode, cookie, q string, onlyPrimary bool) ([]Section, error) {
	extra := map[string]any{"query": q}
	if onlyPrimary {
		if mode == ModeYouTube {
			extra["params"] = videosFilter
		} else {
			extra["params"] = songsFilter
		}
	}
	res, err := y.call(ctx, mode, cookie, "search", extra)
	if err != nil {
		return nil, err
	}
	return y.parse(mode, res, "Videos", false), nil
}

// Queue returns the "up next" list for a video (auto-mix/radio) or for a
// playlist/mix id. Same shape in both modes.
func (y *YTM) Queue(ctx context.Context, mode Mode, cookie, videoID, playlistID string) ([]Section, error) {
	extra := map[string]any{}
	if videoID != "" {
		extra["videoId"] = videoID
	}
	switch {
	case playlistID != "":
		extra["playlistId"] = playlistID
	case mode == ModeYouTube:
		extra["playlistId"] = "RD" + videoID
	default:
		extra["playlistId"] = "RDAMVM" + videoID
		extra["params"] = "wAEB"
	}
	if mode == ModeMusic {
		extra["enablePersistentPlaylistPanel"] = true
		extra["isAudioOnly"] = true
		extra["tunerSettingValue"] = "AUTOMIX_SETTING_NORMAL"
	}
	res, err := y.call(ctx, mode, cookie, "next", extra)
	if err != nil {
		return nil, err
	}
	var items []Item
	collectPanel(res, &items)
	items = dedupe(items)
	if len(items) == 0 {
		return nil, nil
	}
	return []Section{{Title: "Up next", Items: items}}, nil
}

// CreatePlaylist creates a real playlist on the user's YouTube account.
func (y *YTM) CreatePlaylist(ctx context.Context, mode Mode, cookie, title, privacy string, ids []string) (string, error) {
	body := map[string]any{"title": title, "description": "", "privacyStatus": privacy}
	if len(ids) > 0 {
		body["videoIds"] = ids
	}
	res, err := y.call(ctx, mode, cookie, "playlist/create", body)
	if err != nil {
		return "", err
	}
	id := str(res["playlistId"])
	if id == "" {
		return "", fmt.Errorf("youtube did not return a playlist id")
	}
	return id, nil
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
	it := &Item{Title: col(0), Subtitle: col(1), Thumb: findThumb(m["thumbnail"]), Duration: responsiveDuration(m)}
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
