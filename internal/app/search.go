package app

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/css"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

var archiveLink = regexp.MustCompile(`/archives/([A-Z0-9]+)/p(\d{10})(\d{6})(?:[^0-9]|$)`)

type hit struct{ channel, ts, key, href string }

func parseHit(href string) (hit, error) {
	parts := archiveLink.FindStringSubmatch(href)
	if len(parts) != 4 {
		return hit{}, errors.New("could not parse a search result permalink; capture incomplete")
	}
	return hit{parts[1], parts[2] + "." + parts[3], parts[1] + "-" + parts[2] + parts[3], href}, nil
}

func (b *Browser) openSearch(workspace, query, domain string) error {
	if err := b.navigate(workspace); err != nil {
		return err
	}
	if err := b.auth(workspace, domain); err != nil {
		return err
	}
	if err := b.dismissAvailabilityReminder(); err != nil {
		return err
	}
	if err := b.closeStaleThreads(); err != nil {
		return err
	}
	input := `[data-qa="focusable_search_input"] [data-qa="texty_input"]`
	if n, _ := b.nodes(input, 0); len(n) == 0 {
		if err := b.click(`[data-qa="top_nav_search"]`); err != nil {
			return fmt.Errorf("could not open the search input: %w", err)
		}
	}
	if err := b.typeInto(input, query); err != nil {
		return fmt.Errorf("could not enter the search query: %w", err)
	}
	if err := b.click(`[role="option"]:has([data-qa="search-query-entity-text-content"])`); err != nil {
		return fmt.Errorf("could not select the search suggestion: %w", err)
	}
	if err := b.wait(`[data-qa="search_view"]`, 20*time.Second); err != nil {
		return fmt.Errorf("search results did not open: %w", err)
	}
	previous, stable := "", 0
	for range 100 {
		nav, e := chromedp.Run(b.ctx, chromedp.Text(`[data-qa="top_nav_search"]`))
		if e == nil && strings.Contains(nav, query) {
			items, e := b.results()
			if e == nil && len(items) > 0 {
				sig := fmt.Sprintf("%d:%s:%s", len(items), items[0].href, items[len(items)-1].href)
				if sig == previous {
					stable++
				} else {
					stable = 0
				}
				previous = sig
				if stable >= 3 {
					return nil
				}
			} else if empty, e := b.nodes(`[data-qa="empty_state_wrapper"]`, 0); e == nil && len(empty) > 0 {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("search results did not finish loading")
}

func (b *Browser) results() ([]hit, error) {
	for range 10 {
		items, err := b.nodes(`[data-qa="search_result"]`, 0)
		if err != nil {
			return nil, err
		}
		found := make([]hit, 0, len(items))
		stale := false
		for _, item := range items {
			links, e := b.nodes(`a[href*="/archives/"][href*="/p"]`, item)
			if e != nil {
				if strings.Contains(e.Error(), "Could not find node") {
					stale = true
					break
				}
				return nil, e
			}
			if len(links) == 0 {
				stale = true
				break
			}
			href, e := b.attr(links[0], "href")
			if e != nil {
				if strings.Contains(e.Error(), "Could not find node") {
					stale = true
					break
				}
				return nil, e
			}
			h, e := parseHit(href)
			if e != nil {
				return nil, e
			}
			found = append(found, h)
		}
		if !stale {
			return found, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, errors.New("search results did not stabilize")
}

func (b *Browser) nextPage(previous string) (bool, error) {
	buttons, err := b.nodes(`[data-qa="c-pagination_forward_btn"]`, 0)
	if err != nil || len(buttons) == 0 {
		return false, err
	}
	if disabled, e := b.hasAttr(buttons[0], "disabled"); e != nil {
		return false, e
	} else if disabled {
		return false, nil
	}
	if v, _ := b.attr(buttons[0], "aria-disabled"); v == "true" {
		return false, nil
	}
	if err = b.clickID(buttons[0]); err != nil {
		return false, err
	}
	for range 75 {
		items, e := b.results()
		if e == nil && len(items) > 0 && items[0].href != previous {
			return true, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false, errors.New("could not load the next search results page")
}

func (b *Browser) restoreSearch(workspace, domain string, o Options, pageNo int) error {
	if n, e := b.nodes(`[data-qa="search_result"], [data-qa="empty_state_wrapper"]`, 0); e == nil && len(n) > 0 {
		return nil
	}
	if err := b.openSearch(workspace, o.Query, domain); err != nil {
		return err
	}
	// ponytail: restoring page N rescans previous pages; upgrade only if this becomes a measured bottleneck.
	for range pageNo {
		hits, err := b.results()
		if err != nil || len(hits) == 0 {
			return errors.New("could not restore the search results page")
		}
		if ok, err := b.nextPage(hits[0].href); err != nil || !ok {
			return errors.New("could not restore the search results page")
		}
	}
	return nil
}

func (b *Browser) captureSearch(workspace string, o Options, out *Output) (int, error) {
	seen := map[string]bool{}
	completed := 0
	for pageNo := range 1000 {
		hits, err := b.results()
		if err != nil {
			return completed, err
		}
		refresh := 0
		for cursor := 0; cursor < len(hits); cursor++ {
			h := hits[cursor]
			if seen[h.key] {
				continue
			}
			if err = b.restoreSearch(workspace, o.Domain, o, pageNo); err != nil {
				return completed, err
			}
			items, err := b.nodes(`[data-qa="search_result"]`, 0)
			if err != nil {
				return completed, err
			}
			var chosen cdp.NodeID
			for _, item := range items {
				links, e := b.nodes(`a[href*="/archives/"][href*="/p"]`, item)
				if e != nil || len(links) == 0 {
					continue
				}
				href, e := b.attr(links[0], "href")
				if e == nil && href == h.href {
					chosen = item
					break
				}
			}
			if chosen == 0 {
				if refresh++; refresh > 5 {
					return completed, errors.New("search results keep changing; capture incomplete")
				}
				fmt.Fprintln(os.Stderr, "Search results changed; refreshing the list")
				time.Sleep(300 * time.Millisecond)
				hits, err = b.results()
				if err != nil {
					return completed, err
				}
				cursor = -1
				continue
			}
			seen[h.key] = true
			if err = b.hoverID(chosen); err != nil {
				return completed, err
			}
			var jump cdp.NodeID
			for range 10 {
				jump, err = b.jumpFor(h.ts)
				if err != nil {
					return completed, err
				}
				if jump != 0 {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if jump != 0 {
				if err = b.clickID(jump); err != nil {
					return completed, err
				}
			} else if err = b.clickID(chosen); err != nil {
				return completed, err
			}
			ready := false
			for attempt := 0; attempt < 50 && !ready; attempt++ {
				for _, root := range []string{`[data-qa="threads_flexpane"]`, `[data-qa="message_pane"]`} {
					matches, e := b.nodes(fmt.Sprintf(`%s [data-qa="message_container"][data-msg-ts="%s"]`, root, h.ts), 0)
					if e != nil {
						return completed, e
					}
					if len(matches) > 0 {
						ready = true
						break
					}
				}
				if ready {
					break
				}
				button, e := b.jumpFor(h.ts)
				if e != nil {
					return completed, e
				}
				if button != 0 {
					if e = b.clickID(button); e != nil {
						return completed, e
					}
					ready = true
				}
				if !ready {
					time.Sleep(100 * time.Millisecond)
				}
			}
			if !ready {
				return completed, fmt.Errorf("could not open search hit %s", h.ts)
			}
			dir := filepath.Join(out.Stage, h.key)
			if err = b.captureContext(h.ts, dir, o, out); err != nil {
				return completed, err
			}
			completed++
			fmt.Printf("Search hits captured: %d\n", completed)
			if o.Limit > 0 && completed >= o.Limit {
				return completed, nil
			}
		}
		if err = b.restoreSearch(workspace, o.Domain, o, pageNo); err != nil {
			return completed, err
		}
		if len(hits) == 0 {
			return completed, nil
		}
		ok, err := b.nextPage(hits[0].href)
		if err != nil {
			return completed, err
		}
		if !ok {
			return completed, nil
		}
	}
	return completed, errors.New("search results page limit reached; capture incomplete")
}

func (b *Browser) jumpFor(ts string) (cdp.NodeID, error) {
	buttons, err := b.nodes(`[data-qa="jump_to_message"]`, 0)
	if err != nil {
		return 0, err
	}
	for _, button := range buttons {
		metadata, e := b.attr(button, "data-focus-metadata")
		if e == nil && strings.Contains(metadata, `"ts":"`+ts+`"`) {
			return button, nil
		}
	}
	return 0, nil
}

func (b *Browser) targetPane(ts string) (cdp.NodeID, cdp.NodeID, bool, error) {
	sel := fmt.Sprintf(`[data-qa="message_container"][data-msg-ts="%s"]`, ts)
	for range 100 {
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
	return 0, 0, false, fmt.Errorf("could not position search hit %s in the screenshot viewport", ts)
}

func (b *Browser) captureContext(ts, directory string, o Options, out *Output) error {
	pane, _, thread, err := b.targetPane(ts)
	if err != nil {
		return err
	}
	var sheet cdp.StyleSheetID
	if thread {
		sheet, err = b.isolateThread()
		if err != nil {
			return err
		}
		defer func() {
			_, _ = chromedp.Call(b.ctx, css.SetStyleSheetText, css.SetStyleSheetTextParams{StyleSheetID: sheet, Text: ""})
		}()
	}
	before := map[string]bool{}
	for i := 0; i < 2000 && len(before) < o.Before; i++ {
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
		if e = b.wheel(pane, -math.Max(1, rect.height()*0.9)); e != nil {
			return e
		}
		fresh, e := b.visible(pane, nil)
		if e != nil {
			return e
		}
		if signature(fresh) == prev {
			break
		}
	}
	after := map[string]bool{}
	covered := false
	last, stalled := "", 0
	for i := range 2000 {
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
			if err = os.MkdirAll(directory, 0700); err != nil {
				return err
			}
			var clip *page.Viewport
			if thread {
				height := float64(b.height)
				info, e := chromedp.Call(b.ctx, dom.DescribeNode, dom.DescribeNodeParams{NodeID: pane})
				if e != nil {
					return e
				}
				if info.Node != nil && !info.Node.IsScrollable {
					height = math.Min(height, math.Max(300, math.Ceil(msg[len(msg)-1].rect.y2+10)))
				}
				clip = &page.Viewport{Width: float64(b.width), Height: height, Scale: 1}
			}
			shot, e := b.save(out, pane, clip, filepath.Join(directory, fmt.Sprintf("%06d.png", i+1)))
			if e != nil {
				return e
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
