package main

import (
	"regexp"
	"strconv"
	"strings"
)

var timeRe = regexp.MustCompile(`^\d{1,2}(:\d{2}){1,2}$`)

// parseDuration turns "3:45" / "1:02:03" into seconds (0 if not a time).
func parseDuration(s string) int {
	s = strings.TrimSpace(s)
	if !timeRe.MatchString(s) {
		return 0
	}
	total := 0
	for _, p := range strings.Split(s, ":") {
		n, _ := strconv.Atoi(p)
		total = total*60 + n
	}
	return total
}

func textOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if s := str(dig(v, "simpleText")); s != "" {
		return s
	}
	return runsText(v)
}

func isRadioID(id string) bool { return strings.HasPrefix(id, "RD") }

func dedupe(items []Item) []Item {
	seen := map[string]bool{}
	var out []Item
	for _, it := range items {
		if it.ID == "" || seen[it.ID] {
			continue
		}
		seen[it.ID] = true
		out = append(out, it)
	}
	return out
}

// music search results carry the length as the last run of the 2nd column
// (or in the fixed column for playlist rows).
func responsiveDuration(m map[string]any) int {
	var texts []string
	for _, c := range []any{
		dig(m, "fixedColumns", 0, "musicResponsiveListItemFixedColumnRenderer", "text"),
		dig(m, "flexColumns", 1, "musicResponsiveListItemFlexColumnRenderer", "text"),
	} {
		runs, _ := dig(c, "runs").([]any)
		for _, r := range runs {
			texts = append(texts, str(dig(r, "text")))
		}
	}
	for i := len(texts) - 1; i >= 0; i-- {
		if d := parseDuration(texts[i]); d > 0 {
			return d
		}
	}
	return 0
}

// ---------- queue / mix panels (same renderer in both modes) ----------

func parsePanelItem(m map[string]any) *Item {
	id := str(m["videoId"])
	if id == "" {
		return nil
	}
	by := textOf(m["longBylineText"])
	if by == "" {
		by = textOf(m["shortBylineText"])
	}
	return &Item{Type: "song", ID: id, Title: textOf(m["title"]), Subtitle: by,
		Thumb: findThumb(m["thumbnail"]), Duration: parseDuration(textOf(m["lengthText"]))}
}

func collectPanel(v any, out *[]Item) {
	switch t := v.(type) {
	case map[string]any:
		if w, ok := t["playlistPanelVideoWrapperRenderer"].(map[string]any); ok {
			if r, ok := dig(w, "primaryRenderer", "playlistPanelVideoRenderer").(map[string]any); ok {
				if it := parsePanelItem(r); it != nil {
					*out = append(*out, *it)
				}
			}
			return
		}
		if r, ok := t["playlistPanelVideoRenderer"].(map[string]any); ok {
			if it := parsePanelItem(r); it != nil {
				*out = append(*out, *it)
			}
			return
		}
		for _, c := range t {
			collectPanel(c, out)
		}
	case []any:
		for _, c := range t {
			collectPanel(c, out)
		}
	}
}

// ---------- regular YouTube result parsing ----------

type ytCollected struct{ videos, mixes, playlists, channels []Item }

func ytVideo(r map[string]any) *Item {
	id := str(r["videoId"])
	if id == "" || r["upcomingEventData"] != nil {
		return nil
	}
	ch := textOf(r["ownerText"])
	if ch == "" {
		ch = textOf(r["shortBylineText"])
	}
	if ch == "" {
		ch = textOf(r["longBylineText"])
	}
	sub := ch
	if v := textOf(r["shortViewCountText"]); v != "" {
		if sub != "" {
			sub += " • "
		}
		sub += v
	}
	dur := parseDuration(textOf(r["lengthText"]))
	if dur == 0 {
		dur, _ = strconv.Atoi(str(r["lengthSeconds"]))
	}
	if dur == 0 && strings.Contains(textOf(r["viewCountText"]), "watching") {
		return nil // live stream
	}
	return &Item{Type: "song", ID: id, Title: textOf(r["title"]), Subtitle: sub,
		Thumb: findThumb(r["thumbnail"]), Duration: dur}
}

func ytPlaylistItem(r map[string]any) *Item {
	id := str(r["playlistId"])
	if id == "" {
		return nil
	}
	t := "playlist"
	if isRadioID(id) {
		t = "mix"
	}
	return &Item{Type: t, ID: id, Title: textOf(r["title"]), Subtitle: textOf(r["shortBylineText"]), Thumb: findThumb(r)}
}

func ytChannel(r map[string]any) *Item {
	id := str(r["channelId"])
	if id == "" {
		return nil
	}
	return &Item{Type: "artist", ID: id, Title: textOf(r["title"]), Subtitle: textOf(r["subscriberCountText"]), Thumb: findThumb(r)}
}

func findSourceURL(v any) string {
	switch t := v.(type) {
	case map[string]any:
		if s, ok := t["sources"].([]any); ok && len(s) > 0 {
			return str(dig(s[len(s)-1], "url"))
		}
		for _, c := range t {
			if u := findSourceURL(c); u != "" {
				return u
			}
		}
	case []any:
		for _, c := range t {
			if u := findSourceURL(c); u != "" {
				return u
			}
		}
	}
	return ""
}

func findDuration(v any) int {
	switch t := v.(type) {
	case string:
		return parseDuration(t)
	case map[string]any:
		for _, c := range t {
			if d := findDuration(c); d > 0 {
				return d
			}
		}
	case []any:
		for _, c := range t {
			if d := findDuration(c); d > 0 {
				return d
			}
		}
	}
	return 0
}

// New-style "lockup" cards (used by the YouTube home feed and mixes).
func (c *ytCollected) addLockup(r map[string]any) {
	id := str(r["contentId"])
	if id == "" {
		return
	}
	var parts []string
	rows, _ := dig(r, "metadata", "lockupMetadataViewModel", "metadata", "contentMetadataViewModel", "metadataRows").([]any)
	for _, row := range rows {
		ps, _ := dig(row, "metadataParts").([]any)
		for _, p := range ps {
			if s := str(dig(p, "text", "content")); s != "" {
				parts = append(parts, s)
			}
		}
	}
	if len(parts) > 2 {
		parts = parts[:2]
	}
	it := Item{ID: id, Title: str(dig(r, "metadata", "lockupMetadataViewModel", "title", "content")),
		Subtitle: strings.Join(parts, " • "), Thumb: findSourceURL(r["contentImage"]), Duration: findDuration(r["contentImage"])}
	switch str(r["contentType"]) {
	case "LOCKUP_CONTENT_TYPE_VIDEO":
		it.Type = "song"
		c.videos = append(c.videos, it)
	case "LOCKUP_CONTENT_TYPE_PLAYLIST":
		it.Type = "playlist"
		if isRadioID(id) {
			it.Type = "mix"
			c.mixes = append(c.mixes, it)
		} else {
			c.playlists = append(c.playlists, it)
		}
	}
}

func walkYT(v any, c *ytCollected) {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range []string{"adSlotRenderer", "promotedSparklesWebRenderer", "reelItemRenderer"} {
			if _, ok := t[k]; ok {
				return
			}
		}
		for _, k := range []string{"videoRenderer", "compactVideoRenderer", "gridVideoRenderer", "playlistVideoRenderer"} {
			if r, ok := t[k].(map[string]any); ok {
				if it := ytVideo(r); it != nil {
					c.videos = append(c.videos, *it)
				}
				return
			}
		}
		if r, ok := t["lockupViewModel"].(map[string]any); ok {
			c.addLockup(r)
			return
		}
		for _, k := range []string{"playlistRenderer", "gridPlaylistRenderer", "compactPlaylistRenderer", "radioRenderer", "compactRadioRenderer", "gridRadioRenderer"} {
			if r, ok := t[k].(map[string]any); ok {
				if it := ytPlaylistItem(r); it != nil {
					if it.Type == "mix" {
						c.mixes = append(c.mixes, *it)
					} else {
						c.playlists = append(c.playlists, *it)
					}
				}
				return
			}
		}
		for _, k := range []string{"channelRenderer", "gridChannelRenderer", "compactChannelRenderer"} {
			if r, ok := t[k].(map[string]any); ok {
				if it := ytChannel(r); it != nil {
					c.channels = append(c.channels, *it)
				}
				return
			}
		}
		for _, ch := range t {
			walkYT(ch, c)
		}
	case []any:
		for _, ch := range t {
			walkYT(ch, c)
		}
	}
}

func (c *ytCollected) sections(videoTitle string, mixesFirst bool) []Section {
	var out []Section
	add := func(title string, items []Item) {
		if items = dedupe(items); len(items) > 0 {
			out = append(out, Section{Title: title, Items: items})
		}
	}
	if mixesFirst {
		add("Mixes for you", c.mixes)
	}
	add(videoTitle, c.videos)
	if !mixesFirst {
		add("Mixes", c.mixes)
	}
	add("Playlists", c.playlists)
	add("Channels", c.channels)
	return out
}
