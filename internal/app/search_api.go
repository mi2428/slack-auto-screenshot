package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const keychainTokenLabel = "slack-auto-screenshot-user-token"

var archiveLink = regexp.MustCompile(`/archives/([A-Z0-9]+)/p(\d{10})(\d{6})(?:[^0-9]|$)`)
var messageTimestamp = regexp.MustCompile(`^\d{10}\.\d{6}$`)
var slackURL = regexp.MustCompile(`(?i)<https?://[^>]+>|https?://[^\s<]+`)
var slackChannel = regexp.MustCompile(`^[A-Z0-9]+$`)
var savedFileID = regexp.MustCompile(`^F[A-Z0-9]+$`)

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

func searchTotal(r slackSearchResponse) (int, error) {
	total := 0
	for _, value := range []int{r.Messages.Total, r.Messages.Pagination.TotalCount, r.Messages.Paging.Total} {
		if value < 0 {
			return 0, errors.New("slack search returned a negative total")
		}
		if value == 0 {
			continue
		}
		if total != 0 && value != total {
			return 0, errors.New("slack search returned inconsistent totals")
		}
		total = value
	}
	return total, nil
}

func searchPages(r slackSearchResponse, total int) (int, error) {
	pages := 0
	for _, value := range []int{r.Messages.Pagination.PageCount, r.Messages.Paging.Pages} {
		if value < 0 {
			return 0, errors.New("slack search returned a negative page count")
		}
		if value == 0 {
			continue
		}
		if pages != 0 && value != pages {
			return 0, errors.New("slack search returned inconsistent page counts")
		}
		pages = value
	}
	if pages == 0 {
		pages = (total + 99) / 100
	} else if total > 0 && pages != (total+99)/100 {
		return 0, errors.New("slack search page count does not match its total")
	}
	return pages, nil
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
		currentTotal, err := searchTotal(response)
		if err != nil {
			return nil, 0, err
		}
		currentPages, err := searchPages(response, currentTotal)
		if err != nil {
			return nil, 0, err
		}
		if total < 0 {
			total = currentTotal
			pages = currentPages
			if pages > 100 {
				return nil, 0, errors.New("slack search exceeds the 10,000-result API limit")
			}
		} else if currentTotal != total {
			return nil, 0, errors.New("slack search total changed during pagination")
		} else if currentPages != pages {
			return nil, 0, errors.New("slack search page count changed during pagination")
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
