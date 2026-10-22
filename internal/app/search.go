package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

const keychainTokenLabel = "slack-auto-screenshot-user-token"
const searchHitAttempts = 3
const searchStateVersion = 2
const searchNavigationTimeout = 30 * time.Second
const searchContextTimeout = 5 * time.Minute

var archiveLink = regexp.MustCompile(`/archives/([A-Z0-9]+)/p(\d{10})(\d{6})(?:[^0-9]|$)`)
var messageTimestamp = regexp.MustCompile(`^\d{10}\.\d{6}$`)
var slackURL = regexp.MustCompile(`(?i)<https?://[^>]+>|https?://[^\s<]+`)
var slackChannel = regexp.MustCompile(`^[A-Z0-9]+$`)
var savedFileID = regexp.MustCompile(`^F[A-Z0-9]+$`)

type hit struct {
	Channel   string `json:"channel"`
	TS        string `json:"ts"`
	Parent    string `json:"parent"`
	Key       string `json:"key"`
	Permalink string `json:"-"`
}

type searchSpec struct {
	Domain, ExcludeAuthor, ExcludeLeading, ExcludeURLSubstring string
	Theme                                                      string
	Width, Height, OutputWidth, Before, After, Limit, Offset   int
	Scale                                                      float64
	OldestFirst                                                bool
}

type searchState struct {
	Version                       int
	Spec                          searchSpec
	Stage                         string
	Total, Targets, ThreadReplies int
	LeadingExcluded, URLExcluded  int
	Hits                          []hit
	Completed, Dates, Saved       map[string]bool
	Missing                       map[string]MissingFile
}

type capturedSearchContext struct {
	Directory string
	Messages  map[string]bool
}

type slackAuth struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error"`
	URL    string `json:"url"`
	User   string `json:"user"`
	UserID string `json:"user_id"`
}

type slackMatch struct {
	Permalink string `json:"permalink"`
	Text      string `json:"text"`
	TS        string `json:"ts"`
	User      string `json:"user"`
}

type slackSearchResponse struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Messages struct {
		Matches    []slackMatch `json:"matches"`
		Total      int          `json:"total"`
		Pagination struct {
			PageCount  int `json:"page_count"`
			TotalCount int `json:"total_count"`
		} `json:"pagination"`
		Paging struct {
			Pages int `json:"pages"`
			Total int `json:"total"`
		} `json:"paging"`
	} `json:"messages"`
}

type slackAPI struct {
	baseURL string
	token   string
	client  *http.Client
}

func keychainArgs(domain string) []string {
	return []string{"find-internet-password", "-l", keychainTokenLabel, "-s", domain, "-w"}
}

func keychainToken(ctx context.Context, domain string) (string, error) {
	out, err := exec.CommandContext(ctx, "security", keychainArgs(domain)...).Output()
	if err != nil {
		return "", fmt.Errorf("slack user token not found in Keychain: create an Internet Password named %s with Website %s", keychainTokenLabel, domain)
	}
	token := strings.TrimSpace(string(out))
	if !strings.HasPrefix(token, "xoxp-") {
		return "", errors.New("keychain item does not contain a Slack user token (xoxp-)")
	}
	return token, nil
}

func (a slackAPI) get(ctx context.Context, method string, params url.Values, dst any) error {
	endpoint := a.baseURL + method
	if encoded := params.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	for range 5 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+a.token)
		resp, err := a.client.Do(req)
		if err != nil {
			return fmt.Errorf("slack API %s: %w", method, err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			_ = resp.Body.Close()
			seconds, err := strconv.Atoi(resp.Header.Get("Retry-After"))
			if err != nil || seconds < 0 || seconds > 300 {
				return fmt.Errorf("slack API %s returned an invalid Retry-After", method)
			}
			fmt.Fprintf(os.Stderr, "Slack API rate limited; waiting %d second(s)\n", seconds)
			timer := time.NewTimer(time.Duration(seconds) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return fmt.Errorf("slack API %s returned HTTP %d", method, resp.StatusCode)
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(dst)
		_ = resp.Body.Close()
		if err != nil {
			return fmt.Errorf("decode Slack API %s response: %w", method, err)
		}
		return nil
	}
	return fmt.Errorf("slack API %s remained rate limited", method)
}

func (a slackAPI) authenticate(ctx context.Context, domain string) (slackAuth, error) {
	var auth slackAuth
	if err := a.get(ctx, "auth.test", nil, &auth); err != nil {
		return auth, err
	}
	if !auth.OK {
		return auth, fmt.Errorf("slack auth.test: %s", auth.Error)
	}
	u, err := url.Parse(auth.URL)
	if err != nil {
		return auth, fmt.Errorf("slack auth.test returned an invalid workspace URL: %w", err)
	}
	if u.Scheme != "https" || u.User != nil || !strings.EqualFold(u.Host, domain) {
		return auth, fmt.Errorf("keychain token belongs to %s, not %s", u.Hostname(), domain)
	}
	if auth.User == "" || auth.UserID == "" {
		return auth, errors.New("slack auth.test did not identify the user")
	}
	return auth, nil
}

func searchTotal(r slackSearchResponse) int {
	for _, total := range []int{r.Messages.Total, r.Messages.Pagination.TotalCount, r.Messages.Paging.Total} {
		if total > 0 {
			return total
		}
	}
	return 0
}

func searchPages(r slackSearchResponse, total int) int {
	for _, pages := range []int{r.Messages.Pagination.PageCount, r.Messages.Paging.Pages} {
		if pages > 0 {
			return pages
		}
	}
	return (total + 99) / 100
}

func (a slackAPI) search(ctx context.Context, query string, oldestFirst bool) ([]slackMatch, int, error) {
	seen := map[string]bool{}
	var matches []slackMatch
	total, pages := -1, 1
	for pageNo := 1; pageNo <= pages; pageNo++ {
		if pageNo > 100 {
			return nil, 0, errors.New("slack search exceeds the 10,000-result API limit")
		}
		sortDirection := "desc"
		if oldestFirst {
			sortDirection = "asc"
		}
		params := url.Values{
			"query":    {query},
			"count":    {"100"},
			"page":     {strconv.Itoa(pageNo)},
			"sort":     {"timestamp"},
			"sort_dir": {sortDirection},
		}
		var response slackSearchResponse
		if err := a.get(ctx, "search.messages", params, &response); err != nil {
			return nil, 0, err
		}
		if !response.OK {
			return nil, 0, fmt.Errorf("slack search.messages: %s", response.Error)
		}
		currentTotal := searchTotal(response)
		if total < 0 {
			total = currentTotal
			pages = searchPages(response, total)
			if pages > 100 {
				return nil, 0, errors.New("slack search exceeds the 10,000-result API limit")
			}
		} else if currentTotal != total {
			return nil, 0, errors.New("slack search total changed during pagination")
		}
		for _, match := range response.Messages.Matches {
			key := match.Permalink
			if key == "" {
				key = match.TS
			}
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			matches = append(matches, match)
		}
	}
	if total < 0 {
		total = 0
	}
	if len(matches) != total {
		return nil, total, fmt.Errorf("slack API reported %d hits but returned %d unique messages", total, len(matches))
	}
	return matches, total, nil
}

func searchQuery(o Options, auth slackAuth) (string, error) {
	query := o.Query
	for flag, value := range map[string]string{"--exclude-author": o.ExcludeAuthor, "--exclude-leading-mention": o.ExcludeLeading} {
		if value != "" && !strings.EqualFold(value, auth.User) {
			return "", fmt.Errorf("%s must name the authenticated Slack user @%s", flag, auth.User)
		}
	}
	if o.ExcludeAuthor != "" {
		query += " -from:<@" + auth.UserID + ">"
	}
	return query, nil
}

func startsWithUserMention(text, userID string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "<@"+userID+">") || strings.HasPrefix(text, "<@"+userID+"|")
}

func keywordOnlyInURL(text, keyword string) bool {
	keyword = strings.ToLower(keyword)
	foundInURL := false
	withoutURLs := slackURL.ReplaceAllStringFunc(text, func(link string) string {
		visible := ""
		if inner, ok := strings.CutPrefix(link, "<"); ok {
			parts := strings.SplitN(strings.TrimSuffix(inner, ">"), "|", 2)
			link = parts[0]
			if len(parts) == 2 {
				visible = parts[1]
			}
		}
		foundInURL = foundInURL || strings.Contains(strings.ToLower(link), keyword)
		if visible != "" {
			return " " + visible + " "
		}
		return " "
	})
	return foundInURL && !strings.Contains(strings.ToLower(withoutURLs), keyword)
}

func parseHit(permalink, domain string) (hit, error) {
	u, err := url.Parse(permalink)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), domain) {
		return hit{}, errors.New("slack API returned a permalink outside the requested workspace")
	}
	parts := archiveLink.FindStringSubmatch(u.Path)
	if len(parts) != 4 {
		return hit{}, errors.New("could not parse a Slack search permalink")
	}
	ts := parts[2] + "." + parts[3]
	parent := u.Query().Get("thread_ts")
	if parent == "" {
		parent = ts
	}
	if !messageTimestamp.MatchString(parent) {
		return hit{}, errors.New("slack API returned an invalid thread timestamp")
	}
	if cid := u.Query().Get("cid"); cid != "" && cid != parts[1] {
		return hit{}, errors.New("slack API returned an inconsistent thread channel")
	}
	return hit{Channel: parts[1], TS: ts, Parent: parent, Key: parts[1] + "-" + parts[2] + parts[3], Permalink: permalink}, nil
}

func inventory(ctx context.Context, o Options, api slackAPI) ([]hit, int, int, int, error) {
	auth, err := api.authenticate(ctx, o.Domain)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	query, err := searchQuery(o, auth)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	matches, total, err := api.search(ctx, query, o.OldestFirst)
	if err != nil {
		return nil, total, 0, 0, err
	}
	hits := make([]hit, 0, len(matches))
	leadingExcluded, urlExcluded := 0, 0
	for _, match := range matches {
		if o.ExcludeAuthor != "" && match.User == auth.UserID {
			return nil, total, leadingExcluded, urlExcluded, errors.New("slack API returned a message by the excluded author")
		}
		if o.ExcludeLeading != "" && startsWithUserMention(match.Text, auth.UserID) {
			leadingExcluded++
			continue
		}
		if o.ExcludeURLSubstring != "" && keywordOnlyInURL(match.Text, o.ExcludeURLSubstring) {
			urlExcluded++
			continue
		}
		h, err := parseHit(match.Permalink, o.Domain)
		if err != nil {
			return nil, total, leadingExcluded, urlExcluded, err
		}
		hits = append(hits, h)
	}
	return hits, total, leadingExcluded, urlExcluded, nil
}

func searchOptions(o Options) searchSpec {
	return searchSpec{
		Domain: o.Domain, ExcludeAuthor: o.ExcludeAuthor, ExcludeLeading: o.ExcludeLeading,
		ExcludeURLSubstring: o.ExcludeURLSubstring, Theme: o.Theme, Width: o.Width, Height: o.Height,
		OutputWidth: o.OutputWidth, Before: o.Before, After: o.After, Limit: o.Limit, Offset: o.Offset, Scale: o.Scale,
		OldestFirst: o.OldestFirst,
	}
}

func searchStatePath(o Options) (string, error) {
	// Keep this fingerprint encoding stable so existing interrupted searches can resume.
	data, err := json.Marshal(struct {
		Domain, Query, ExcludeAuthor, ExcludeLeading, ExcludeURLSubstring string
		Theme                                                             string
		Width, Height, OutputWidth, Before, After, Limit, Offset          int
		Scale                                                             float64
		OldestFirst                                                       bool
	}{
		Domain: o.Domain, Query: o.Query, ExcludeAuthor: o.ExcludeAuthor, ExcludeLeading: o.ExcludeLeading,
		ExcludeURLSubstring: o.ExcludeURLSubstring, Theme: o.Theme, Width: o.Width, Height: o.Height,
		OutputWidth: o.OutputWidth, Before: o.Before, After: o.After, Limit: o.Limit, Offset: o.Offset,
		Scale: o.Scale, OldestFirst: o.OldestFirst,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return filepath.Join(o.Output, fmt.Sprintf(".search-%x.json", sum[:8])), nil
}

func saveSearchState(path string, state *searchState, out *Output) error {
	state.Dates, state.Saved, state.Missing = out.Dates, out.Saved, out.Missing
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".search-state-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func validStoredHit(h hit) bool {
	return slackChannel.MatchString(h.Channel) && messageTimestamp.MatchString(h.TS) &&
		messageTimestamp.MatchString(h.Parent) && h.Key == h.Channel+"-"+strings.ReplaceAll(h.TS, ".", "") && h.Permalink == ""
}

func validateSearchScreens(stage string, h hit, o Options) error {
	directory := filepath.Join(stage, h.Key)
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("completed search hit %s has no screenshot directory", h.Key)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	scale := o.Scale
	if o.OutputWidth != 0 {
		scale = float64(o.OutputWidth) / float64(o.Width)
	}
	wantWidth, wantHeight := int(math.Round(float64(o.Width)*scale)), int(math.Round(float64(o.Height)*scale))
	frames := 0
	for _, entry := range entries {
		frames++
		wantName := fmt.Sprintf("%06d.png", frames)
		if entry.Name() != wantName || !entry.Type().IsRegular() {
			return fmt.Errorf("completed search hit %s contains an invalid screenshot", h.Key)
		}
		file, err := os.Open(filepath.Join(directory, entry.Name()))
		if err != nil {
			return err
		}
		config, format, decodeErr := image.DecodeConfig(file)
		closeErr := file.Close()
		if decodeErr != nil || format != "png" || config.Width != wantWidth || config.Height != wantHeight {
			return fmt.Errorf("completed search hit %s contains an invalid PNG", h.Key)
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if frames == 0 {
		return fmt.Errorf("completed search hit %s has no screenshots", h.Key)
	}
	return nil
}

func loadSearchState(path string, o Options) (*searchState, *Output, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	var state searchState
	if err = json.Unmarshal(data, &state); err != nil {
		return nil, nil, false, fmt.Errorf("read search resume state: %w", err)
	}
	if state.Version != searchStateVersion || state.Spec != searchOptions(o) {
		return nil, nil, false, errors.New("search resume state does not match this command")
	}
	stage := filepath.Clean(state.Stage)
	if filepath.Dir(stage) != filepath.Clean(o.Output) || !strings.HasPrefix(filepath.Base(stage), ".incomplete-") {
		return nil, nil, false, errors.New("search resume state contains an invalid capture directory")
	}
	info, err := os.Lstat(stage)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, false, errors.New("search resume capture directory is unavailable")
	}
	keys := map[string]hit{}
	for _, h := range state.Hits {
		if _, duplicate := keys[h.Key]; !validStoredHit(h) || duplicate {
			return nil, nil, false, errors.New("search resume state contains an invalid hit")
		}
		keys[h.Key] = h
	}
	for key := range state.Completed {
		h, ok := keys[key]
		if !ok {
			return nil, nil, false, errors.New("search resume state contains an unknown completed hit")
		}
		if err = validateSearchScreens(stage, h, o); err != nil {
			return nil, nil, false, fmt.Errorf("search resume state is incomplete: %w", err)
		}
	}
	if state.Completed == nil {
		state.Completed = map[string]bool{}
	}
	if state.Dates == nil {
		state.Dates = map[string]bool{}
	}
	if state.Saved == nil {
		state.Saved = map[string]bool{}
	}
	if state.Missing == nil {
		state.Missing = map[string]MissingFile{}
	}
	entries, err := os.ReadDir(stage)
	if err != nil {
		return nil, nil, false, err
	}
	foundSaved := map[string]bool{}
	for _, entry := range entries {
		name := strings.TrimPrefix(entry.Name(), "file_")
		if entry.Type().IsRegular() && name != entry.Name() {
			if end := strings.IndexByte(name, '_'); end > 1 && name[0] == 'F' {
				foundSaved[name[:end]] = true
				state.Saved[name[:end]] = true
			}
		}
	}
	for key := range state.Saved {
		if !savedFileID.MatchString(key) || !foundSaved[key] {
			return nil, nil, false, fmt.Errorf("search resume state references a missing attachment: %s", key)
		}
	}
	out := &Output{Root: o.Output, Stage: stage, Dates: state.Dates, Saved: state.Saved, Missing: state.Missing}
	return &state, out, true, nil
}

func searchContextRoute(h hit) string {
	if h.Parent == h.TS {
		return "channel:" + h.Channel
	}
	return "thread:" + h.Channel + ":" + h.Parent
}

func groupSearchHits(hits []hit) ([]hit, int, int) {
	groups := map[string][]hit{}
	var order []string
	for _, h := range hits {
		key := searchContextRoute(h)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], h)
	}
	grouped := make([]hit, 0, len(hits))
	samePane, reusedThread := 0, 0
	for _, key := range order {
		grouped = append(grouped, groups[key]...)
		if len(groups[key]) > 1 {
			samePane += len(groups[key]) - 1
			if groups[key][0].Parent != groups[key][0].TS {
				reusedThread += len(groups[key]) - 1
			}
		}
	}
	return grouped, samePane, reusedThread
}

func contextCovers(messages map[string]bool, h hit, before, after int) bool {
	if !messages[h.TS] {
		return false
	}
	older, newer := 0, 0
	for ts := range messages {
		if ts < h.TS {
			older++
		} else if ts > h.TS {
			newer++
		}
	}
	return older >= before && newer >= after
}

func searchMessagesOverlap(a, b []message) bool {
	for _, old := range a {
		for _, current := range b {
			if old.ts == current.ts {
				return true
			}
		}
	}
	return false
}

func linkSearchScreens(source, target string) error {
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	if err = os.RemoveAll(target); err != nil {
		return err
	}
	if err = os.MkdirAll(target, 0700); err != nil {
		return err
	}
	linked := 0
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".png") {
			if err = os.Link(filepath.Join(source, entry.Name()), filepath.Join(target, entry.Name())); err != nil {
				return err
			}
			linked++
		}
	}
	if linked == 0 {
		return errors.New("reusable search context has no screenshots")
	}
	return nil
}

func runSearch(o Options) error {
	if err := os.MkdirAll(o.Output, 0700); err != nil {
		return err
	}
	statePath, err := searchStatePath(o)
	if err != nil {
		return err
	}
	state, out, resumed, err := loadSearchState(statePath, o)
	if err != nil {
		return err
	}
	if !resumed {
		ctx := context.Background()
		inventoryStarted := time.Now()
		token, err := keychainToken(ctx, o.Domain)
		if err != nil {
			return err
		}
		api := slackAPI{baseURL: "https://slack.com/api/", token: token, client: &http.Client{Timeout: 30 * time.Second}}
		hits, total, leadingExcluded, urlExcluded, err := inventory(ctx, o, api)
		api.token = ""
		if err != nil {
			return err
		}
		fmt.Printf("Search inventory: %d ms\n", time.Since(inventoryStarted).Milliseconds())
		threadReplies := 0
		for _, h := range hits {
			if h.Parent != h.TS {
				threadReplies++
			}
		}
		targets := len(hits)
		if o.Offset >= len(hits) {
			hits = nil
		} else if o.Offset > 0 {
			hits = hits[o.Offset:]
		}
		if o.Limit > 0 && len(hits) > o.Limit {
			hits = hits[:o.Limit]
		}
		if len(hits) == 0 {
			fmt.Println("No search hits matched the capture filters")
			return nil
		}
		if !o.OldestFirst {
			hits, _, _ = groupSearchHits(hits)
		}
		out, err = NewOutput(o.Output)
		if err != nil {
			return err
		}
		state = &searchState{
			Version: searchStateVersion, Spec: searchOptions(o), Stage: out.Stage, Total: total, Targets: targets,
			ThreadReplies: threadReplies, LeadingExcluded: leadingExcluded, URLExcluded: urlExcluded, Hits: hits,
			Completed: map[string]bool{}, Dates: out.Dates, Saved: out.Saved, Missing: out.Missing,
		}
		if err = saveSearchState(statePath, state, out); err != nil {
			return err
		}
	} else {
		fmt.Printf("Resuming search capture: %d/%d already complete\n", len(state.Completed), len(state.Hits))
	}
	_, samePaneCandidates, reuseCandidates := groupSearchHits(state.Hits)
	fmt.Printf("Search API hits: %d; leading mentions excluded: %d; URL-only hits excluded: %d; capture targets: %d; thread replies: %d\n", state.Total, state.LeadingExcluded, state.URLExcluded, state.Targets, state.ThreadReplies)
	fmt.Printf("Thread navigations reusable: %d\n", reuseCandidates)
	fmt.Printf("Same-pane context candidates: %d\n", samePaneCandidates)
	fmt.Println("Capture destination (incomplete):", out.Stage)

	remaining := len(state.Hits) - len(state.Completed)
	if remaining > 0 {
		browserStarted := time.Now()
		b, err := OpenBrowser(o, false)
		if err != nil {
			return err
		}
		defer b.Close()
		if err = b.setup(); err != nil {
			return err
		}
		workspace, err := b.workspace(o.Domain)
		if err != nil {
			return err
		}
		if err = b.dismissAvailabilityReminder(); err != nil {
			return err
		}
		if err = b.closeStaleThreads(); err != nil {
			return err
		}
		fmt.Printf("Search browser setup: %d ms\n", time.Since(browserStarted).Milliseconds())
		captureStarted := time.Now()
		remaining, err = b.captureSearchHits(workspace, o, out, state, statePath)
		if err != nil {
			return err
		}
		fmt.Printf("Search capture batch: %d ms\n", time.Since(captureStarted).Milliseconds())
	}
	if remaining > 0 {
		return fmt.Errorf("%d search hit(s) remain incomplete; rerun the same command to retry only them", remaining)
	}
	summary := fmt.Sprintf("Search API hits: %d\nLeading mentions excluded: %d\nURL-only hits excluded: %d\nSearch hits captured: %d\nAttachments: %d saved, %d missing", state.Total, state.LeadingExcluded, state.URLExcluded, len(state.Completed), len(out.Saved), len(out.Missing))
	finishStarted := time.Now()
	path, err := out.Finish("search", summary, o.Gzip)
	if err != nil {
		return err
	}
	if err = os.Remove(statePath); err != nil {
		return fmt.Errorf("capture completed at %s but resume state could not be removed: %w", path, err)
	}
	fmt.Printf("Search finalization: %d ms\n", time.Since(finishStarted).Milliseconds())
	fmt.Printf("%s\n%s\n", summary, path)
	if o.Gzip {
		fmt.Println("Archive:", path+".tar.gz")
	}
	return nil
}

func searchHitWorklist(hits []hit, completed map[string]bool, channelMessagesOnly bool) ([]int, int) {
	var pending []int
	remaining := 0
	for i, h := range hits {
		if completed[h.Key] {
			continue
		}
		remaining++
		if !channelMessagesOnly || h.Parent == h.TS {
			pending = append(pending, i)
		}
	}
	return pending, remaining
}

func (b *Browser) captureSearchHits(workspace string, o Options, out *Output, state *searchState, statePath string) (int, error) {
	pending, _ := searchHitWorklist(state.Hits, state.Completed, o.ChannelMessagesOnly)
	contexts := map[string][]capturedSearchContext{}
	run := func(items []int, firstAttempt, lastAttempt int) ([]int, error) {
		var failed []int
		activeRoute := ""
		for _, i := range items {
			h := state.Hits[i]
			contextRoute := searchContextRoute(h)
			reused := false
			for _, captured := range contexts[contextRoute] {
				if !contextCovers(captured.Messages, h, o.Before, o.After) {
					continue
				}
				if err := linkSearchScreens(captured.Directory, filepath.Join(out.Stage, h.Key)); err != nil {
					return nil, err
				}
				fmt.Printf("Reused captured context for search hit %d/%d\n", i+1, len(state.Hits))
				reused = true
				break
			}
			var messages map[string]bool
			var err error
			if !reused {
				messages, err = b.captureSearchHit(workspace, h, i, len(state.Hits), o, out, contextRoute == activeRoute, firstAttempt, lastAttempt)
			}
			if err != nil {
				return nil, err
			}
			if !reused && messages == nil {
				activeRoute = ""
				failed = append(failed, i)
				continue
			}
			activeRoute = contextRoute
			if !reused {
				contexts[contextRoute] = append(contexts[contextRoute], capturedSearchContext{Directory: filepath.Join(out.Stage, h.Key), Messages: messages})
			}
			state.Completed[h.Key] = true
			if err = saveSearchState(statePath, state, out); err != nil {
				return nil, err
			}
			fmt.Printf("Search hits captured: %d/%d\n", len(state.Completed), len(state.Hits))
		}
		return failed, nil
	}
	failed, err := run(pending, 1, 1)
	if err != nil {
		return len(pending), err
	}
	if len(failed) > 0 {
		fmt.Printf("Retrying %d deferred search hit(s)\n", len(failed))
		failed, err = run(failed, 2, searchHitAttempts)
		if err != nil {
			return len(failed), err
		}
	}
	for _, i := range failed {
		fmt.Fprintf(os.Stderr, "WARNING: Search hit %d/%d remains incomplete\n", i+1, len(state.Hits))
	}
	_, remaining := searchHitWorklist(state.Hits, state.Completed, false)
	return remaining, saveSearchState(statePath, state, out)
}

type searchAttemptState struct {
	dates   map[string]bool
	saved   map[string]bool
	missing map[string]MissingFile
}

func snapshotSearchAttempt(out *Output) searchAttemptState {
	return searchAttemptState{dates: maps.Clone(out.Dates), saved: maps.Clone(out.Saved), missing: maps.Clone(out.Missing)}
}

func rollbackSearchAttempt(out *Output, before searchAttemptState, directory string) error {
	entries, err := os.ReadDir(out.Stage)
	if err != nil {
		return err
	}
	for key := range out.Saved {
		if before.saved[key] {
			continue
		}
		prefix := "file_" + key + "_"
		for _, entry := range entries {
			if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), prefix) {
				if err = os.Remove(filepath.Join(out.Stage, entry.Name())); err != nil {
					return err
				}
			}
		}
	}
	out.Dates, out.Saved, out.Missing = before.dates, before.saved, before.missing
	return os.RemoveAll(directory)
}

func (b *Browser) captureSearchHit(workspace string, h hit, index, total int, o Options, out *Output, reusePane bool, firstAttempt, lastAttempt int) (map[string]bool, error) {
	kind := "channel message"
	if h.Parent != h.TS {
		kind = "thread reply"
	}
	fmt.Printf("Opening search hit %d/%d (%s)\n", index+1, total, kind)
	var captureErr error
	directory := filepath.Join(out.Stage, h.Key)
	for attempt := firstAttempt; attempt <= lastAttempt; attempt++ {
		captureErr = nil
		before := snapshotSearchAttempt(out)
		messages := map[string]bool{}
		if err := os.RemoveAll(directory); err != nil {
			return nil, err
		}
		if reusePane && attempt == firstAttempt {
			fmt.Println("Reusing open message pane")
			captureErr = b.captureContext(h.TS, h.Parent, directory, o, out, true, messages)
			if captureErr == nil {
				return messages, nil
			}
		}
		if captureErr != nil && attempt == firstAttempt {
			fmt.Fprintf(os.Stderr, "Open thread reuse failed; reopening: %v\n", captureErr)
			if err := rollbackSearchAttempt(out, before, directory); err != nil {
				return nil, err
			}
			messages = map[string]bool{}
		}
		appURL := workspace + "/" + h.Channel + "/thread/" + h.Channel + "-" + h.Parent
		if h.Parent != h.TS {
			appURL += "?thread_ts=" + url.QueryEscape(h.Parent) + "&cid=" + url.QueryEscape(h.Channel)
		}
		navigationStarted := time.Now()
		navigationErr := b.navigateWithin(appURL, searchNavigationTimeout)
		fmt.Printf("Search navigation: %d ms\n", time.Since(navigationStarted).Milliseconds())
		if navigationErr != nil {
			captureErr = fmt.Errorf("open search hit: %w", navigationErr)
			_ = b.navigate("about:blank")
		} else {
			if err := b.auth(workspace, o.Domain); err != nil {
				if !errors.Is(err, context.DeadlineExceeded) {
					return nil, err
				}
				captureErr = fmt.Errorf("wait for search page readiness: %w", err)
			}
			if captureErr == nil {
				fmt.Println("Search hit page opened")
			}
			if captureErr == nil && h.Parent == h.TS {
				prepareStarted := time.Now()
				captureErr = b.prepareChannelHit(h.TS)
				fmt.Printf("Search channel preparation: %d ms\n", time.Since(prepareStarted).Milliseconds())
			}
			if captureErr == nil {
				captureErr = b.captureContext(h.TS, h.Parent, directory, o, out, false, messages)
			}
		}
		if captureErr == nil {
			return messages, nil
		}
		if err := rollbackSearchAttempt(out, before, directory); err != nil {
			return nil, err
		}
		if attempt < lastAttempt {
			fmt.Fprintf(os.Stderr, "Search hit capture failed; retrying (%d/%d): %v\n", attempt+1, searchHitAttempts, captureErr)
		}
	}
	fmt.Fprintf(os.Stderr, "Search hit deferred after attempt %d: %v\n", lastAttempt, captureErr)
	return nil, nil
}

func (b *Browser) targetPane(ts string) (cdp.NodeID, cdp.NodeID, bool, error) {
	sel := fmt.Sprintf(`[data-qa="message_container"][data-msg-ts="%s"]`, ts)
	for range 50 {
		for _, root := range []string{`[data-qa="threads_flexpane"]`, `[data-qa="message_pane"]`} {
			roots, e := b.nodes(root, 0)
			if e != nil {
				return 0, 0, false, e
			}
			for _, scope := range roots {
				targets, e := b.nodes(sel, scope)
				if e != nil || len(targets) == 0 {
					continue
				}
				_, e = chromedp.Call(b.ctx, dom.ScrollIntoViewIfNeeded, dom.ScrollIntoViewIfNeededParams{NodeID: targets[0]})
				if e != nil {
					continue
				}
				pane, e := b.findPane(targets[0])
				if e == nil {
					return pane, targets[0], strings.Contains(root, "threads_flexpane"), nil
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if pane, err := b.paneFor(`[data-qa="threads_flexpane"] [data-qa="message_container"]`); err == nil {
		fmt.Println("Search hit not initially loaded; scanning thread pane")
		if err = b.positionPaneMessage(pane, ts); err == nil {
			targets, e := b.nodes(sel, pane)
			if e == nil && len(targets) > 0 {
				return pane, targets[0], true, nil
			}
		}
	}
	if pane, err := b.paneFor(`[data-qa="message_pane"] [data-qa="message_container"]`); err == nil {
		fmt.Println("Search hit not initially loaded; scanning main pane")
		if err = b.positionPaneMessage(pane, ts); err == nil {
			targets, e := b.nodes(sel, pane)
			if e == nil && len(targets) > 0 {
				return pane, targets[0], false, nil
			}
		}
	}
	return 0, 0, false, fmt.Errorf("could not position search hit %s in the screenshot viewport", ts)
}

func (b *Browser) positionMainParent(parent string) (cdp.NodeID, error) {
	return b.positionMainParentWithin(parent, 200)
}

func (b *Browser) positionMainParentWithin(parent string, limit int) (cdp.NodeID, error) {
	pane, err := b.paneFor(`[data-qa="message_pane"] [data-qa="message_container"]`)
	if err != nil {
		return 0, err
	}
	selector := fmt.Sprintf(`[data-qa="message_container"][data-msg-ts="%s"]`, parent)
	if parents, err := b.nodes(selector, pane); err != nil {
		return 0, err
	} else if len(parents) > 0 {
		if _, err = chromedp.Call(b.ctx, dom.ScrollIntoViewIfNeeded, dom.ScrollIntoViewIfNeededParams{NodeID: parents[0]}); err != nil {
			return 0, err
		}
		return pane, nil
	}
	last, stalled := "", 0
	for i := range limit {
		parents, err := b.nodes(selector, pane)
		if err != nil {
			return 0, err
		}
		if len(parents) > 0 {
			if _, err = chromedp.Call(b.ctx, dom.ScrollIntoViewIfNeeded, dom.ScrollIntoViewIfNeededParams{NodeID: parents[0]}); err != nil {
				return 0, err
			}
			return pane, nil
		}
		messages, err := b.visible(pane, nil)
		if err != nil {
			return 0, err
		}
		if len(messages) == 0 {
			return 0, errors.New("main message pane has no visible messages")
		}
		now := signature(messages)
		if now == last {
			stalled++
		} else {
			stalled = 0
		}
		if stalled >= 5 {
			return 0, errors.New("thread parent was not found in the main message pane")
		}
		last = now
		if i > 0 && i%20 == 0 {
			fmt.Printf("Positioning parent in main pane: %d scrolls\n", i)
		}
		rect, err := b.box(pane)
		if err != nil {
			return 0, err
		}
		delta := -math.Max(1, rect.height()*3)
		if messages[len(messages)-1].ts < parent {
			delta = -delta
		}
		if err = b.wheel(pane, delta); err != nil {
			return 0, err
		}
	}
	return 0, errors.New("thread parent search exceeded its scroll limit")
}

func (b *Browser) prepareChannelHit(ts string) error {
	return b.within(2*time.Minute, func() error {
		pane, _, thread, err := b.targetPane(ts)
		if err != nil {
			return err
		}
		if thread {
			if _, err = b.openThreadParent(pane, ts); err != nil {
				return err
			}
		} else if _, err = b.positionMainParent(ts); err != nil {
			return err
		}
		return b.closeStaleThreads()
	})
}

func (b *Browser) openThreadParent(threadPane cdp.NodeID, parent string) (cdp.NodeID, error) {
	mainPane, err := b.paneFor(`[data-qa="message_pane"] [data-qa="message_container"]`)
	if err != nil {
		return 0, err
	}
	selector := fmt.Sprintf(`[data-qa="message_container"][data-msg-ts="%s"]`, parent)
	if parents, err := b.nodes(selector, mainPane); err != nil {
		return 0, err
	} else if len(parents) > 0 {
		if _, err = chromedp.Call(b.ctx, dom.ScrollIntoViewIfNeeded, dom.ScrollIntoViewIfNeededParams{NodeID: parents[0]}); err != nil {
			return 0, err
		}
		return mainPane, nil
	}
	fmt.Println("Positioning thread parent in main pane")
	if mainPane, err = b.positionMainParentWithin(parent, 40); err == nil {
		return mainPane, nil
	}
	parents, err := b.nodes(selector, threadPane)
	if err != nil {
		return 0, err
	}
	if len(parents) == 0 {
		if err = b.oldest(threadPane); err != nil {
			return 0, err
		}
		parents, err = b.nodes(selector, threadPane)
		if err != nil {
			return 0, err
		}
	}
	if len(parents) != 1 {
		return 0, errors.New("thread parent unavailable in thread pane")
	}
	permalink := fmt.Sprintf(`a[href*="/archives/"][href*="/p%s"]`, strings.ReplaceAll(parent, ".", ""))
	links, err := b.nodes(permalink, parents[0])
	if err != nil {
		return 0, err
	}
	if len(links) != 1 {
		return 0, errors.New("thread parent permalink unavailable")
	}
	fmt.Println("Opening thread parent in channel fallback")
	if err = b.clickID(links[0]); err != nil {
		return 0, err
	}
	for range 100 {
		mainPane, err = b.paneFor(`[data-qa="message_pane"] [data-qa="message_container"]`)
		if err == nil {
			parents, e := b.nodes(selector, mainPane)
			if e != nil {
				return 0, e
			}
			if len(parents) > 0 {
				if _, e = chromedp.Call(b.ctx, dom.ScrollIntoViewIfNeeded, dom.ScrollIntoViewIfNeededParams{NodeID: parents[0]}); e != nil {
					return 0, e
				}
				return mainPane, nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return 0, errors.New("thread parent did not open in channel fallback")
}

func (b *Browser) reopenThreadPane(parent string) (cdp.NodeID, error) {
	pane, err := b.paneFor(`[data-qa="threads_flexpane"] [data-qa="message_container"]`)
	if err == nil {
		return pane, nil
	}
	mainPane, err := b.paneFor(`[data-qa="message_pane"] [data-qa="message_container"]`)
	if err != nil {
		return 0, fmt.Errorf("find main message pane to reopen thread: %w", err)
	}
	selector := fmt.Sprintf(`[data-qa="message_container"][data-msg-ts="%s"]`, parent)
	parents, err := b.nodes(selector, mainPane)
	if err != nil {
		return 0, err
	}
	if len(parents) == 1 {
		parentRect, rectErr := b.box(parents[0])
		if rectErr == nil {
			_, rectErr = chromedp.Call(b.ctx, input.DispatchMouseEvent, input.DispatchMouseEventParams{
				Type: input.DispatchMouseEventTypeMouseMoved,
				X:    parentRect.x1 + parentRect.width()/2,
				Y:    parentRect.y1 + parentRect.height()/2,
			})
			if rectErr == nil {
				time.Sleep(500 * time.Millisecond)
			}
			bars, barsErr := b.nodes(`[data-qa="reply_bar_view_thread"]`, mainPane)
			if barsErr == nil {
				best, distance := cdp.NodeID(0), math.Inf(1)
				for _, bar := range bars {
					r, e := b.box(bar)
					if e != nil || r.y2 < parentRect.y1-20 || r.y1 > parentRect.y2+200 {
						continue
					}
					d := math.Abs(r.y1 - parentRect.y2)
					if d < distance {
						best, distance = bar, d
					}
				}
				if best != 0 && b.clickID(best) == nil {
					if err = b.wait(`[data-qa="threads_flexpane"] [data-qa="message_container"]`, 10*time.Second); err == nil {
						return b.paneFor(`[data-qa="threads_flexpane"] [data-qa="message_container"]`)
					}
				}
			}
		}
		resolved, resolveErr := chromedp.Call(b.ctx, dom.ResolveNode, dom.ResolveNodeParams{NodeID: parents[0]})
		if resolveErr == nil && resolved.Object != nil && resolved.Object.ObjectID != "" {
			userGesture := true
			result, clickErr := chromedp.Call(b.ctx, runtime.CallFunctionOn, runtime.CallFunctionOnParams{
				FunctionDeclaration: `function(){let root=this,bar=null;while(root&&root.getAttribute('data-qa')!=='message_pane'){bar=root.querySelector('[data-qa="reply_bar_view_thread"],button[aria-label*="thread" i],button[aria-label*="reply" i],[role="button"][aria-label*="thread" i],[role="button"][aria-label*="reply" i]');if(bar)break;root=root.parentElement}if(!bar)throw new Error("reply bar not found");bar.click()}`,
				ObjectID:            resolved.Object.ObjectID,
				UserGesture:         &userGesture,
			})
			_, _ = chromedp.Call(b.ctx, runtime.ReleaseObject, runtime.ReleaseObjectParams{ObjectID: resolved.Object.ObjectID})
			if clickErr == nil && result.Result != nil && result.ExceptionDetails == nil {
				if err = b.wait(`[data-qa="threads_flexpane"] [data-qa="message_container"]`, 10*time.Second); err == nil {
					return b.paneFor(`[data-qa="threads_flexpane"] [data-qa="message_container"]`)
				}
			}
		}
	}
	selectors := []string{
		fmt.Sprintf(`[data-qa="virtual-list-item"]:has(%s) [data-qa="reply_bar_view_thread"]`, selector),
		fmt.Sprintf(`%s [data-qa="reply_bar_view_thread"]`, selector),
	}
	var clickErr error
	for _, selector := range selectors {
		if clickErr = b.click(selector); clickErr == nil {
			break
		}
	}
	if clickErr != nil {
		return 0, fmt.Errorf("reopen thread pane: %w", clickErr)
	}
	if err = b.wait(`[data-qa="threads_flexpane"] [data-qa="message_container"]`, 10*time.Second); err != nil {
		return 0, fmt.Errorf("wait for reopened thread pane: %w", err)
	}
	return b.paneFor(`[data-qa="threads_flexpane"] [data-qa="message_container"]`)
}

func (b *Browser) positionPaneMessage(pane cdp.NodeID, target string) error {
	return b.positionPaneMessageWithin(pane, target, 200)
}

func (b *Browser) positionPaneMessageWithin(pane cdp.NodeID, target string, limit int) error {
	selector := fmt.Sprintf(`[data-qa="message_container"][data-msg-ts="%s"]`, target)
	last, stalled := "", 0
	for i := range limit {
		messages, err := b.nodes(selector, pane)
		if err != nil {
			return err
		}
		if len(messages) > 0 {
			_, err = chromedp.Call(b.ctx, dom.ScrollIntoViewIfNeeded, dom.ScrollIntoViewIfNeededParams{NodeID: messages[0]})
			return err
		}
		visible, err := b.visible(pane, nil)
		if err != nil {
			return err
		}
		if len(visible) == 0 {
			return errors.New("message pane has no visible messages")
		}
		now := signature(visible)
		if now == last {
			stalled++
		} else {
			stalled = 0
		}
		if stalled >= 5 {
			return errors.New("target message was not found in its pane")
		}
		last = now
		if i > 0 && i%20 == 0 {
			fmt.Printf("Positioning search hit in thread: %d scrolls\n", i)
		}
		rect, err := b.box(pane)
		if err != nil {
			return err
		}
		delta := -math.Max(1, rect.height()*0.9)
		if visible[len(visible)-1].ts < target {
			delta = -delta
		}
		if err = b.wheel(pane, delta); err != nil {
			return err
		}
	}
	return errors.New("search hit positioning exceeded its scroll limit")
}

func (b *Browser) captureContext(ts, parent, directory string, o Options, out *Output, reusePane bool, captured map[string]bool) error {
	return b.captureContextWithin(ts, parent, directory, o, out, reusePane, captured, searchContextTimeout)
}

func (b *Browser) captureContextWithin(ts, parent, directory string, o Options, out *Output, reusePane bool, captured map[string]bool, timeout time.Duration) error {
	return b.within(timeout, func() error {
		return b.captureContextRun(ts, parent, directory, o, out, reusePane, captured)
	})
}

func (b *Browser) captureContextRun(ts, parent, directory string, o Options, out *Output, reusePane bool, captured map[string]bool) error {
	started := time.Now()
	defer func() { fmt.Printf("Search context capture: %d ms\n", time.Since(started).Milliseconds()) }()
	var pane, mainPane cdp.NodeID
	var thread bool
	err := b.within(2*time.Minute, func() error {
		var err error
		if reusePane {
			if parent == ts {
				pane, err = b.paneFor(`[data-qa="message_pane"] [data-qa="message_container"]`)
				if err != nil {
					return err
				}
				return b.positionPaneMessageWithin(pane, ts, 10)
			}
			thread = true
			mainPane, err = b.paneFor(`[data-qa="message_pane"] [data-qa="message_container"]`)
			if err != nil {
				return err
			}
			pane, err = b.paneFor(`[data-qa="threads_flexpane"] [data-qa="message_container"]`)
			if err != nil {
				return err
			}
			return b.positionPaneMessage(pane, ts)
		}
		pane, _, thread, err = b.targetPane(ts)
		if err != nil {
			return err
		}
		if !thread {
			if parent != ts {
				return errors.New("thread reply is not open in the thread pane")
			}
			return nil
		}
		mainPane, err = b.openThreadParent(pane, parent)
		if err != nil {
			return err
		}
		pane, err = b.reopenThreadPane(parent)
		if err != nil {
			return err
		}
		return b.positionPaneMessage(pane, ts)
	})
	if err != nil {
		return fmt.Errorf("position search hit: %w", err)
	}
	fmt.Println("Search hit positioned")
	before := map[string]bool{}
	for i := 0; i < 100 && len(before) < o.Before; i++ {
		if i > 0 && i%20 == 0 {
			fmt.Printf("Loading context before search hit: %d scrolls\n", i)
		}
		msg, e := b.visible(pane, nil)
		if e != nil {
			return e
		}
		for _, m := range msg {
			if m.ts < ts {
				before[m.ts] = true
			}
		}
		if len(before) >= o.Before {
			break
		}
		rect, e := b.box(pane)
		if e != nil {
			return e
		}
		prev := signature(msg)
		if e = b.wheel(pane, -math.Max(1, rect.height()*0.45)); e != nil {
			return e
		}
		fresh, e := b.visible(pane, nil)
		if e != nil {
			return e
		}
		if len(msg) > 0 && len(fresh) > 0 && !searchMessagesOverlap(msg, fresh) {
			return errors.New("search context lost message overlap while loading earlier messages")
		}
		if signature(fresh) == prev {
			break
		}
	}
	after := map[string]bool{}
	covered := false
	last, stalled, frames := "", 0, 0
	var previous []message
	for i := range 100 {
		if i > 0 && i%20 == 0 {
			fmt.Printf("Capturing context after search hit: %d scrolls\n", i)
		}
		msg, e := b.visible(pane, nil)
		if e != nil {
			return e
		}
		if len(msg) == 0 {
			return errors.New("search hit disappeared from the message pane")
		}
		now := signature(msg)
		if now == last {
			stalled++
		} else {
			stalled = 0
		}
		last = now
		if stalled == 0 {
			if len(previous) > 0 && !searchMessagesOverlap(previous, msg) {
				return errors.New("search context lost screenshot overlap")
			}
			if err = os.MkdirAll(directory, 0700); err != nil {
				return err
			}
			frames++
			shot, e := b.save(out, pane, nil, filepath.Join(directory, fmt.Sprintf("%06d.png", frames)))
			if e != nil {
				return e
			}
			for _, m := range shot {
				if captured != nil {
					captured[m.ts] = true
				}
			}
			previous = shot
			if thread {
				mainMessages, e := b.visible(mainPane, nil)
				if e != nil {
					return e
				}
				parentVisible := false
				for _, m := range mainMessages {
					out.Dates[m.day] = true
					if m.ts == parent {
						parentVisible = true
					}
				}
				if !parentVisible {
					return errors.New("thread parent was not included in the split screenshot")
				}
				if e = b.collectFiles(out, mainMessages); e != nil {
					return e
				}
			}
			for _, m := range shot {
				if m.ts == ts {
					covered = true
				}
				if m.ts > ts {
					after[m.ts] = true
				}
			}
		}
		if covered && len(after) >= o.After {
			return nil
		}
		if stalled >= 5 {
			if !covered {
				return errors.New("search hit was not included in a screenshot")
			}
			return nil
		}
		rect, e := b.box(pane)
		if e != nil {
			return e
		}
		if e = b.wheel(pane, math.Max(1, rect.height()*0.9)); e != nil {
			return e
		}
	}
	return errors.New("search context traversal exceeded its limit")
}
