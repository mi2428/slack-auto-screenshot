package app

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func (b *Browser) targetPane(ts string) (cdp.NodeID, cdp.NodeID, bool, error) {
	sel := slackMessageAt(ts)
	for range 50 {
		for _, root := range []string{slackUIThreadPane, slackUIMessagePane} {
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
	if pane, err := b.paneFor(slackUIThreadMessages); err == nil {
		fmt.Println("Search hit not initially loaded; scanning thread pane")
		if err = b.positionPaneMessage(pane, ts); err == nil {
			targets, e := b.nodes(sel, pane)
			if e == nil && len(targets) > 0 {
				return pane, targets[0], true, nil
			}
		}
	}
	if pane, err := b.paneFor(slackUIConversationMessages); err == nil {
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
	pane, err := b.paneFor(slackUIConversationMessages)
	if err != nil {
		return 0, err
	}
	selector := slackMessageAt(parent)
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
	mainPane, err := b.paneFor(slackUIConversationMessages)
	if err != nil {
		return 0, err
	}
	selector := slackMessageAt(parent)
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
		if err = b.captureStart(threadPane, ""); err != nil {
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
		mainPane, err = b.paneFor(slackUIConversationMessages)
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
	pane, err := b.paneFor(slackUIThreadMessages)
	if err == nil {
		return pane, nil
	}
	mainPane, err := b.paneFor(slackUIConversationMessages)
	if err != nil {
		return 0, fmt.Errorf("find main message pane to reopen thread: %w", err)
	}
	selector := slackMessageAt(parent)
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
			bars, barsErr := b.nodes(slackUIThreadReplyBar, mainPane)
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
					if err = b.wait(slackUIThreadMessages, 10*time.Second); err == nil {
						return b.paneFor(slackUIThreadMessages)
					}
				}
			}
		}
		resolved, resolveErr := chromedp.Call(b.ctx, dom.ResolveNode, dom.ResolveNodeParams{NodeID: parents[0]})
		if resolveErr == nil && resolved.Object != nil && resolved.Object.ObjectID != "" {
			userGesture := true
			result, clickErr := chromedp.Call(b.ctx, runtime.CallFunctionOn, runtime.CallFunctionOnParams{
				FunctionDeclaration: slackReopenThreadClick,
				ObjectID:            resolved.Object.ObjectID,
				UserGesture:         &userGesture,
			})
			_, _ = chromedp.Call(b.ctx, runtime.ReleaseObject, runtime.ReleaseObjectParams{ObjectID: resolved.Object.ObjectID})
			if clickErr == nil && result.Result != nil && result.ExceptionDetails == nil {
				if err = b.wait(slackUIThreadMessages, 10*time.Second); err == nil {
					return b.paneFor(slackUIThreadMessages)
				}
			}
		}
	}
	selectors := []string{
		slackThreadReplyButton(parent),
		selector + " " + slackUIThreadReplyBar,
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
	if err = b.wait(slackUIThreadMessages, 10*time.Second); err != nil {
		return 0, fmt.Errorf("wait for reopened thread pane: %w", err)
	}
	return b.paneFor(slackUIThreadMessages)
}

func (b *Browser) positionPaneMessage(pane cdp.NodeID, target string) error {
	return b.positionPaneMessageWithin(pane, target, 200)
}

func (b *Browser) positionPaneMessageWithin(pane cdp.NodeID, target string, limit int) error {
	selector := slackMessageAt(target)
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
				pane, err = b.paneFor(slackUIConversationMessages)
				if err != nil {
					return err
				}
				return b.positionPaneMessageWithin(pane, ts, 10)
			}
			thread = true
			mainPane, err = b.paneFor(slackUIConversationMessages)
			if err != nil {
				return err
			}
			pane, err = b.paneFor(slackUIThreadMessages)
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
