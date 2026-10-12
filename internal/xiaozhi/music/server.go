package music

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// DefaultServerURL is the music server used when none is configured.
const DefaultServerURL = "https://youtube.kytuoi.com"

// Track is one search hit on the music server.
type Track struct {
	ID    string
	Title string
}

// goodEnoughScore: every query word is in the title — stop trying variants.
const goodEnoughScore = 40

var httpClient = &http.Client{Timeout: 15 * time.Second}

// streamClient has no overall timeout: a song body is read for minutes.
var streamClient = &http.Client{}

// NormalizeServerURL trims, adds https:// and drops trailing slashes.
func NormalizeServerURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return DefaultServerURL
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "https://" + u
	}
	return strings.TrimRight(u, "/")
}

type searchResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Data    []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"data"`
}

// Search tries the query and a few spelling variants, returning the hit whose
// title matches the most query words.
func Search(ctx context.Context, base, query string) (Track, error) {
	base = NormalizeServerURL(base)
	var best Track
	bestScore := -1 << 31
	var lastErr error
	for _, q := range searchCandidates(query) {
		hits, err := searchOnce(ctx, base, q)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				break
			}
			continue
		}
		for _, h := range hits {
			if s := scoreTitle(query, h.Title); s > bestScore {
				bestScore, best = s, h
			}
		}
		if best.ID != "" && bestScore >= goodEnoughScore {
			break
		}
	}
	if best.ID != "" {
		return best, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no results")
	}
	return Track{}, lastErr
}

func searchOnce(ctx context.Context, base, q string) ([]Track, error) {
	u := base + "/api/search?q=" + url.QueryEscape(q) + "&limit=5"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", "WireOS-Vector-Music/1.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return nil, err
	}
	var sr searchResponse
	if jerr := json.Unmarshal(body, &sr); jerr != nil {
		return nil, fmt.Errorf("search HTTP %d: %s", resp.StatusCode, shorten(string(body)))
	}
	if resp.StatusCode != http.StatusOK || !sr.Success {
		msg := sr.Error
		if msg == "" {
			msg = shorten(string(body))
		}
		return nil, fmt.Errorf("search HTTP %d: %s", resp.StatusCode, shorten(msg))
	}
	out := make([]Track, 0, len(sr.Data))
	for _, d := range sr.Data {
		id := strings.TrimSpace(d.ID)
		if id == "" {
			continue
		}
		title := strings.TrimSpace(d.Title)
		if title == "" {
			title = id
		}
		out = append(out, Track{ID: id, Title: title})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no results for %q", q)
	}
	return out, nil
}

// OpenStream starts the MP3 download for a track. The caller closes the body.
func OpenStream(ctx context.Context, base, id string) (io.ReadCloser, error) {
	u := NormalizeServerURL(base) + "/api/stream/mp3?id=" + url.QueryEscape(id) + "&format=mp3"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "audio/mpeg,*/*")
	req.Header.Set("User-Agent", "WireOS-Vector-Music/1.0")
	resp, err := streamClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		msg := string(body)
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, fmt.Errorf("stream HTTP %d: %s", resp.StatusCode, shorten(msg))
	}
	return resp.Body, nil
}

var (
	reLabel = regexp.MustCompile(`(?i)\b(m-?tp|official|mv)\b`)
	reSpace = regexp.MustCompile(`\s+`)
)

// searchCandidates mirrors the Otto player: the music server 500s on some
// spellings ("Sơn Tùng M-TP"), so also try ASCII, label-free and "nhac …" forms.
// Never fall back to a single word (too many wrong hits).
func searchCandidates(query string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(reSpace.ReplaceAllString(s, " "))
		if s == "" || seen[strings.ToLower(s)] {
			return
		}
		seen[strings.ToLower(s)] = true
		out = append(out, s)
	}
	add(query)
	ascii := stripDiacritics(query)
	add(ascii)
	noLabel := strings.TrimSpace(reSpace.ReplaceAllString(reLabel.ReplaceAllString(ascii, " "), " "))
	add(noLabel)
	if noLabel != "" && !strings.HasPrefix(strings.ToLower(noLabel), "nhac ") {
		add("nhac " + noLabel)
	}
	words := strings.Fields(noLabel)
	if len(words) >= 3 {
		add(strings.Join(words[:3], " "))
	}
	if len(words) >= 2 {
		two := strings.Join(words[:2], " ")
		add(two)
		add("nhac " + two)
	}
	return out
}

// scoreTitle prefers titles containing every query word.
func scoreTitle(query, title string) int {
	t := strings.ToLower(stripDiacritics(title))
	var words []string
	for _, w := range strings.Fields(strings.ToLower(stripDiacritics(query))) {
		if len([]rune(w)) >= 2 {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		return 0
	}
	score, hit := 0, 0
	for _, w := range words {
		if strings.Contains(t, w) {
			hit++
			score += 10
		}
	}
	if hit == len(words) {
		score += 30
	}
	if strings.Contains(t, "tap ") && hit < len(words) {
		score -= 5
	}
	return score
}

func stripDiacritics(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, err := transform.String(t, s)
	if err != nil {
		out = s
	}
	return strings.NewReplacer("đ", "d", "Đ", "D").Replace(out)
}

func shorten(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 160 {
		return string(r[:160]) + "…"
	}
	return s
}
