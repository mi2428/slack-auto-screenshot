package app

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/png"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/css"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

type message struct {
	id      cdp.NodeID
	ts, day string
	rect    bounds
}
type frameOptions struct {
	prefix   string
	thread   bool
	from, to string
}

func localDay(ts string) (string, error) {
	parts := strings.Split(ts, ".")
	if len(parts) > 2 || len(parts[0]) != 10 {
		return "", fmt.Errorf("invalid message timestamp: %s", ts)
	}
	secs, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return "", err
	}
	if len(parts) == 2 {
		if len(parts[1]) > 6 || parts[1] == "" {
			return "", fmt.Errorf("invalid message timestamp: %s", ts)
		}
		if _, err = strconv.Atoi(parts[1]); err != nil {
			return "", err
		}
	}
	return time.Unix(secs, 0).Local().Format("2006-01-02"), nil
}

func overlaps(a, b bounds) bool { return a.x2 > b.x1 && a.x1 < b.x2 && a.y2 > b.y1 && a.y1 < b.y2 }

func (b *Browser) visible(pane cdp.NodeID, clip *page.Viewport) ([]message, error) {
	view, err := b.box(pane)
	if err != nil {
		return nil, err
	}
	viewport := bounds{0, 0, float64(b.width), float64(b.height)}
	if clip != nil {
		viewport = bounds{clip.X, clip.Y, clip.X + clip.Width, clip.Y + clip.Height}
	}
	ids, err := b.nodes(`[data-qa="message_container"][data-msg-ts]`, pane)
	if err != nil {
		return nil, err
	}
	var found []message
	for _, id := range ids {
		r, e := b.box(id)
		if e != nil || !overlaps(r, view) || !overlaps(r, viewport) {
			continue
		}
		ts, e := b.attr(id, "data-msg-ts")
		if e != nil {
			return nil, e
		}
		day, e := localDay(ts)
		if e != nil {
			return nil, e
		}
		found = append(found, message{id: id, ts: ts, day: day, rect: r})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].rect.y1 < found[j].rect.y1 })
	return found, nil
}

func signature(msg []message) string {
	if len(msg) == 0 {
		return ""
	}
	return fmt.Sprintf("%s:%.0f/%s:%.0f", msg[0].ts, msg[0].rect.y1, msg[len(msg)-1].ts, msg[len(msg)-1].rect.y2)
}

func (b *Browser) oldest(pane cdp.NodeID) error {
	previous, stable := "", 0
	for i := range 20000 {
		loaded, err := b.nodes(`[data-qa="message_container"][data-msg-ts]`, pane)
		if err != nil {
			return err
		}
		if len(loaded) > 0 {
			_, err = chromedp.Call(b.ctx, dom.ScrollIntoViewIfNeeded, dom.ScrollIntoViewIfNeededParams{NodeID: loaded[0]})
			if err != nil {
				return err
			}
		}
		box, err := b.box(pane)
		if err != nil {
			return err
		}
		if err = b.wheel(pane, -math.Max(500, box.height()*3)); err != nil {
			return err
		}
		time.Sleep(200 * time.Millisecond)
		msg, err := b.visible(pane, nil)
		if err != nil {
			return err
		}
		now := signature(msg)
		if now != "" && now == previous {
			stable++
		} else {
			stable = 0
		}
		previous = now
		if stable >= 5 {
			return nil
		}
		if i > 0 && i%20 == 0 {
			fmt.Printf("Loading older history: %d scrolls\n", i)
		}
	}
	return errors.New("could not reach the oldest available message; capture incomplete")
}

func (b *Browser) save(out *Output, pane cdp.NodeID, clip *page.Viewport, file string) ([]message, error) {
	visible, err := b.visible(pane, clip)
	if err != nil {
		return nil, err
	}
	if len(visible) == 0 {
		return nil, errors.New("no messages in the screenshot viewport")
	}
	data, err := b.screenshot(clip)
	if err != nil {
		return nil, err
	}
	conf, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "png" {
		return nil, fmt.Errorf("invalid PNG screenshot: %w", err)
	}
	height := b.height
	if clip != nil {
		height = int(clip.Height)
	}
	if conf.Width != b.width*b.scale || conf.Height != height*b.scale {
		return nil, fmt.Errorf("unexpected PNG dimensions: %dx%d", conf.Width, conf.Height)
	}
	if err = os.WriteFile(file, data, 0600); err != nil {
		return nil, err
	}
	for _, msg := range visible {
		out.Dates[msg.day] = true
	}
	fmt.Println("Screenshot:", file)
	if err = b.collectFiles(out, visible); err != nil {
		return nil, err
	}
	return visible, nil
}

func (b *Browser) paneFor(selector string) (cdp.NodeID, error) {
	msg, err := b.first(selector, 0)
	if err != nil {
		return 0, err
	}
	return b.findPane(msg)
}

func (b *Browser) scan(pane cdp.NodeID, from, to string) (bool, error) {
	if err := b.oldest(pane); err != nil {
		return false, err
	}
	last, stalled, matching := "", 0, false
	for range 20000 {
		messages, err := b.visible(pane, nil)
		if err != nil {
			return false, err
		}
		for _, m := range messages {
			if m.day >= from && m.day <= to {
				matching = true
			}
		}
		now := signature(messages)
		if now != "" && now == last {
			stalled++
		} else {
			stalled = 0
		}
		last = now
		if stalled >= 5 {
			return matching, nil
		}
		rect, err := b.box(pane)
		if err != nil {
			return false, err
		}
		if err = b.wheel(pane, math.Max(1, rect.height()*0.9)); err != nil {
			return false, err
		}
	}
	return false, errors.New("thread date scan exceeded its limit")
}

func (b *Browser) capturePane(out *Output, pane cdp.NodeID, directory string, opts frameOptions, onFrame func(cdp.NodeID) error) (int, error) {
	if err := b.oldest(pane); err != nil {
		return 0, err
	}
	if opts.from != "" {
		start, err := b.visible(pane, nil)
		if err != nil {
			return 0, err
		}
		if len(start) > 0 {
			fmt.Printf("Oldest visible range: %s to %s\n", start[0].day, start[len(start)-1].day)
		}
	}
	last, stalled, frames := "", 0, 0
	for range 20000 {
		messages, err := b.visible(pane, nil)
		if err != nil {
			return frames, err
		}
		if len(messages) == 0 {
			return frames, errors.New("scroll pane contains no visible messages")
		}
		if opts.to != "" {
			past := true
			for _, m := range messages {
				if m.day <= opts.to {
					past = false
					break
				}
			}
			if past {
				return frames, nil
			}
		}
		now := signature(messages)
		if now != "" && now == last {
			stalled++
		} else {
			stalled = 0
		}
		last = now
		if stalled == 0 {
			capture := true
			if opts.from != "" {
				capture = false
				for _, m := range messages {
					if m.day >= opts.from && m.day <= opts.to {
						capture = true
						break
					}
				}
			}
			if capture {
				if err = os.MkdirAll(directory, 0700); err != nil {
					return frames, err
				}
				var clip *page.Viewport
				if opts.thread {
					height := float64(b.height)
					paneInfo, e := chromedp.Call(b.ctx, dom.DescribeNode, dom.DescribeNodeParams{NodeID: pane})
					if e != nil {
						return frames, e
					}
					if paneInfo.Node != nil && !paneInfo.Node.IsScrollable && len(messages) > 0 && messages[len(messages)-1].rect.y2 < height-1 {
						height = math.Min(height, math.Max(300, math.Ceil(messages[len(messages)-1].rect.y2+10)))
					}
					clip = &page.Viewport{X: 0, Y: 0, Width: float64(b.width), Height: height, Scale: 1}
				}
				frames++
				if _, err = b.save(out, pane, clip, filepath.Join(directory, fmt.Sprintf("%s%06d.png", opts.prefix, frames))); err != nil {
					return frames, err
				}
			}
			if onFrame != nil {
				if err = onFrame(pane); err != nil {
					return frames, err
				}
			}
		}
		if stalled >= 5 {
			return frames, nil
		}
		rect, err := b.box(pane)
		if err != nil {
			return frames, err
		}
		if err = b.wheel(pane, math.Max(1, rect.height()*0.9)); err != nil {
			return frames, err
		}
	}
	return frames, errors.New("screenshot limit reached; capture incomplete")
}

func (b *Browser) isolateThread() (cdp.StyleSheetID, error) {
	if err := b.wait(`[data-qa="threads_flexpane"]`, 20*time.Second); err != nil {
		return "", err
	}
	threads, err := b.nodes(`[data-qa="threads_flexpane"]`, 0)
	if err != nil {
		return "", err
	}
	if len(threads) != 1 {
		return "", errors.New("could not isolate a unique thread pane")
	}
	background, err := b.style(threads[0], "--sk_primary_background")
	if err != nil {
		return "", err
	}
	for part := range strings.SplitSeq(background, ",") {
		if _, err = strconv.Atoi(strings.TrimSpace(part)); err != nil {
			return "", errors.New("could not determine the thread background color")
		}
	}
	return b.styleSheet(`[data-qa="threads_flexpane"] {position:fixed!important;inset:0 auto auto 0!important;width:100vw!important;height:100vh!important;z-index:100000!important;background:rgb(` + background + `)!important}.p-tab_rail,.p-workspace_banner__desktop-notifications{display:none!important}`)
}

func (b *Browser) threadCallback(out *Output, o Options) func(cdp.NodeID) error {
	seen := map[string]bool{}
	return func(pane cdp.NodeID) error {
		items, err := b.nodes(`[data-qa="virtual-list-item"]:has([data-qa="reply_bar_view_thread"])`, pane)
		if err != nil {
			return err
		}
		view, err := b.box(pane)
		if err != nil {
			return err
		}
		for _, item := range items {
			bar, err := b.nodes(`[data-qa="reply_bar_view_thread"]`, item)
			if err != nil || len(bar) == 0 {
				continue
			}
			r, err := b.box(bar[0])
			if err != nil || !overlaps(r, view) {
				continue
			}
			parents, err := b.nodes(`[data-qa="message_container"][data-msg-ts]`, item)
			if err != nil || len(parents) == 0 {
				return errors.New("thread parent message not found")
			}
			ts, err := b.attr(parents[0], "data-msg-ts")
			if err != nil {
				return err
			}
			if seen[ts] {
				continue
			}
			if err = b.clickID(bar[0]); err != nil {
				return err
			}
			if err = b.wait(`[data-qa="threads_flexpane"] [data-qa="message_container"]`, 20*time.Second); err != nil {
				return err
			}
			thread, err := b.paneFor(`[data-qa="threads_flexpane"] [data-qa="message_container"]`)
			if err != nil {
				return err
			}
			relevant := true
			if o.From != "" {
				relevant, err = b.scan(thread, o.From, o.To)
				if err != nil {
					return err
				}
			}
			if relevant {
				sheet, err := b.isolateThread()
				if err != nil {
					return err
				}
				_, err = b.capturePane(out, thread, out.Stage, frameOptions{prefix: "thread_" + ts + "_", thread: true}, nil)
				_, reset := chromedp.Call(b.ctx, css.SetStyleSheetText, css.SetStyleSheetTextParams{StyleSheetID: sheet, Text: ""})
				if err != nil {
					return err
				}
				if reset != nil {
					return reset
				}
			}
			active, e := b.first(`[data-qa="threads_flexpane"]`, 0)
			if e != nil {
				return e
			}
			if err = b.click(`[data-qa="threads_flexpane"] [data-qa="close_flexpane"]`); err != nil {
				return err
			}
			if err = b.waitThreadClosed(active); err != nil {
				return err
			}
			seen[ts] = true
			fmt.Printf("Threads checked: %d\n", len(seen))
		}
		return nil
	}
}

func (b *Browser) captureConversation(o Options, out *Output) (int, error) {
	pane, err := b.paneFor(`[data-qa="message_pane"] [data-qa="message_container"]`)
	if err != nil {
		return 0, err
	}
	_, err = b.styleSheet(`[data-qa="search_tip_banner"]{display:none!important}`)
	if err != nil {
		return 0, err
	}
	return b.capturePane(out, pane, out.Stage, frameOptions{from: o.From, to: o.To}, b.threadCallback(out, o))
}
