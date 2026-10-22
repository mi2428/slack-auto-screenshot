package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const searchHitAttempts = 3
const searchStateVersion = 2
const searchNavigationTimeout = 30 * time.Second
const searchContextTimeout = 5 * time.Minute

type hit struct {
	Channel   string `json:"channel"`
	TS        string `json:"ts"`
	Parent    string `json:"parent"`
	Key       string `json:"key"`
	Permalink string `json:"-"`
}

type capturedSearchContext struct {
	Directory string
	Messages  map[string]bool
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
			captureErr = b.captureContextAt(workspace, h.Channel, h.TS, h.Parent, directory, o, out, true, messages)
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
				captureErr = b.captureContextAt(workspace, h.Channel, h.TS, h.Parent, directory, o, out, false, messages)
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
