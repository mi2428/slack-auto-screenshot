package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/css"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

type Browser struct {
	ctx                  context.Context
	close                func()
	width, height, scale int
	theme                string
}

func OpenBrowser(o Options, visible bool) (*Browser, error) {
	profile, err := filepath.Abs(".slack-browser")
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(profile, 0700); err != nil {
		return nil, err
	}
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts, chromedp.UserDataDir(profile))
	if visible {
		opts = append(opts, chromedp.VisibleWindow)
	}
	alloc, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancelTab := chromedp.NewContext(alloc)
	if err = chromedp.Do(ctx); err != nil {
		cancelTab()
		cancelAlloc()
		return nil, err
	}
	if _, err = chromedp.Call(ctx, dom.Enable, dom.EnableParams{}); err != nil {
		cancelTab()
		cancelAlloc()
		return nil, err
	}
	return &Browser{ctx: ctx, close: func() { cancelTab(); cancelAlloc() }, width: o.Width, height: o.Height, scale: o.Scale, theme: o.Theme}, nil
}

func (b *Browser) Close() { b.close() }

func (b *Browser) setup() error {
	_, err := chromedp.Call(b.ctx, emulation.SetDeviceMetricsOverride, emulation.SetDeviceMetricsOverrideParams{
		Width: int64(b.width), Height: int64(b.height), DeviceScaleFactor: float64(b.scale), Mobile: false,
	})
	return err
}

func (b *Browser) navigate(addr string) error {
	return chromedp.Do(b.ctx, chromedp.Navigate(addr))
}
func (b *Browser) location() (string, error) { return chromedp.Run(b.ctx, chromedp.Location()) }

func (b *Browser) wait(sel string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(b.ctx, timeout)
	defer cancel()
	return chromedp.Do(ctx, chromedp.WaitVisible(sel))
}

func (b *Browser) nodes(sel string, parent cdp.NodeID) ([]cdp.NodeID, error) {
	if parent == 0 {
		nodes, err := chromedp.Run(b.ctx, chromedp.Nodes(sel, chromedp.AtLeast(0)))
		if err != nil {
			return nil, err
		}
		ids := make([]cdp.NodeID, len(nodes))
		for i, n := range nodes {
			ids[i] = n.NodeID
		}
		return ids, nil
	}
	list, err := chromedp.Call(b.ctx, dom.QuerySelectorAll, dom.QuerySelectorAllParams{NodeID: parent, Selector: sel})
	if err != nil {
		return nil, err
	}
	return list.NodeIDs, nil
}

func (b *Browser) first(sel string, parent cdp.NodeID) (cdp.NodeID, error) {
	nodes, err := b.nodes(sel, parent)
	if err != nil {
		return 0, err
	}
	if len(nodes) == 0 {
		return 0, fmt.Errorf("screen element not found: %s", sel)
	}
	return nodes[0], nil
}

func (b *Browser) attr(id cdp.NodeID, name string) (string, error) {
	r, err := chromedp.Call(b.ctx, dom.GetAttributes, dom.GetAttributesParams{NodeID: id})
	if err != nil {
		return "", err
	}
	for i := 0; i+1 < len(r.Attributes); i += 2 {
		if r.Attributes[i] == name {
			return r.Attributes[i+1], nil
		}
	}
	return "", nil
}

func (b *Browser) hasAttr(id cdp.NodeID, name string) (bool, error) {
	r, err := chromedp.Call(b.ctx, dom.GetAttributes, dom.GetAttributesParams{NodeID: id})
	if err != nil {
		return false, err
	}
	for i := 0; i+1 < len(r.Attributes); i += 2 {
		if r.Attributes[i] == name {
			return true, nil
		}
	}
	return false, nil
}

func (b *Browser) style(id cdp.NodeID, name string) (string, error) {
	r, err := chromedp.Call(b.ctx, css.GetComputedStyleForNode, css.GetComputedStyleForNodeParams{NodeID: id})
	if err != nil {
		return "", err
	}
	for _, v := range r.ComputedStyle {
		if v.Name == name {
			return v.Value, nil
		}
	}
	return "", nil
}

type bounds struct{ x1, y1, x2, y2 float64 }

func (r bounds) width() float64  { return r.x2 - r.x1 }
func (r bounds) height() float64 { return r.y2 - r.y1 }

func (b *Browser) box(id cdp.NodeID) (bounds, error) {
	r, err := chromedp.Call(b.ctx, dom.GetBoxModel, dom.GetBoxModelParams{NodeID: id})
	if err != nil || r.Model == nil {
		return bounds{}, fmt.Errorf("could not locate message on screen: %w", err)
	}
	q := r.Model.Border
	if len(q) != 8 {
		return bounds{}, errors.New("invalid message bounds")
	}
	out := bounds{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	for i := 0; i < 8; i += 2 {
		out.x1 = math.Min(out.x1, q[i])
		out.x2 = math.Max(out.x2, q[i])
		out.y1 = math.Min(out.y1, q[i+1])
		out.y2 = math.Max(out.y2, q[i+1])
	}
	return out, nil
}

func (b *Browser) wheel(pane cdp.NodeID, delta float64) error {
	var before string
	if delta > 0 {
		messages, err := b.visible(pane, nil)
		if err != nil {
			return err
		}
		before = signature(messages)
	}
	box, err := b.box(pane)
	if err != nil {
		return err
	}
	_, err = chromedp.Call(b.ctx, input.DispatchMouseEvent, input.DispatchMouseEventParams{Type: input.DispatchMouseEventTypeMouseWheel, X: math.Max(1, math.Min(float64(b.width-1), box.x1+box.width()/2)), Y: math.Max(1, math.Min(float64(b.height-1), box.y1+box.height()/2)), DeltaX: math.SmallestNonzeroFloat64, DeltaY: delta})
	if err != nil {
		return err
	}
	time.Sleep(700 * time.Millisecond)
	if delta > 0 {
		messages, err := b.visible(pane, nil)
		if err != nil {
			return err
		}
		if signature(messages) == before {
			ids, err := b.nodes(`[data-qa="message_container"][data-msg-ts]`, pane)
			if err != nil {
				return err
			}
			var next cdp.NodeID
			for _, id := range ids {
				r, e := b.box(id)
				if e != nil || r.y1 <= box.y2 {
					continue
				}
				next = id
				break
			}
			if next != 0 {
				if _, err = chromedp.Call(b.ctx, dom.ScrollIntoViewIfNeeded, dom.ScrollIntoViewIfNeededParams{NodeID: next}); err != nil {
					return err
				}
				time.Sleep(250 * time.Millisecond)
			}
		}
	}
	return nil
}

func (b *Browser) click(sel string) error {
	for range 60 {
		ids, err := b.nodes(sel, 0)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, e := b.box(id); e != nil {
				continue
			}
			if e := b.clickID(id); e == nil {
				return nil
			} else if !strings.Contains(e.Error(), "Could not find node") && !strings.Contains(e.Error(), "Could not compute box model") {
				return e
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("click target not found: %s", sel)
}

func (b *Browser) typeInto(sel, text string) error {
	ctx, cancel := context.WithTimeout(b.ctx, 12*time.Second)
	defer cancel()
	if err := chromedp.Do(ctx, chromedp.Click(sel)); err != nil {
		return err
	}
	_, err := chromedp.Call(ctx, input.DispatchKeyEvent, input.DispatchKeyEventParams{Type: input.DispatchKeyEventTypeRawKeyDown, Key: "a", Code: "KeyA", WindowsVirtualKeyCode: 65, Commands: []string{"selectAll"}})
	if err != nil {
		return err
	}
	_, err = chromedp.Call(ctx, input.DispatchKeyEvent, input.DispatchKeyEventParams{Type: input.DispatchKeyEventTypeKeyUp, Key: "a", Code: "KeyA", WindowsVirtualKeyCode: 65})
	if err != nil {
		return err
	}
	return chromedp.Do(ctx, chromedp.SendKeys(sel, kb.Backspace+text))
}

func (b *Browser) findPane(message cdp.NodeID) (cdp.NodeID, error) {
	ts, err := b.attr(message, "data-msg-ts")
	if err != nil {
		return 0, fmt.Errorf("message timestamp: %w", err)
	}
	all, err := b.nodes(fmt.Sprintf(`[data-qa="slack_kit_scrollbar"]:has([data-qa="message_container"][data-msg-ts="%s"])`, ts), 0)
	if err != nil {
		return 0, err
	}
	var pane cdp.NodeID
	minArea := math.Inf(1)
	for _, id := range all {
		overflow, e := b.style(id, "overflow-y")
		if e != nil || overflow != "auto" && overflow != "scroll" {
			continue
		}
		r, e := b.box(id)
		if e != nil {
			continue
		}
		contained, e := b.nodes(fmt.Sprintf(`[data-qa="message_container"][data-msg-ts="%s"]`, ts), id)
		if e != nil {
			continue
		}
		selected := slices.Contains(contained, message)
		if selected && r.width()*r.height() < minArea {
			pane = id
			minArea = r.width() * r.height()
		}
	}
	if pane == 0 {
		return 0, errors.New("message scroll pane not found")
	}
	return pane, nil
}

func (b *Browser) setTheme() error {
	if b.theme == "" {
		return nil
	}
	id, err := b.first("body", 0)
	if err != nil {
		return err
	}
	cls, err := b.attr(id, "class")
	if err != nil {
		return err
	}
	if !strings.Contains(cls, "sk-client-theme--") {
		return errors.New("capture page is missing the Slack theme class")
	}
	cls = strings.ReplaceAll(strings.ReplaceAll(cls, "sk-client-theme--dark", ""), "sk-client-theme--light", "")
	_, err = chromedp.Call(b.ctx, dom.SetAttributeValue, dom.SetAttributeValueParams{NodeID: id, Name: "class", Value: strings.TrimSpace(cls) + " sk-client-theme--" + b.theme})
	return err
}

func (b *Browser) styleSheet(rule string) (cdp.StyleSheetID, error) {
	_, err := chromedp.Call(b.ctx, css.Enable, cdp.Empty{})
	if err != nil {
		return "", err
	}
	tree, err := chromedp.Call(b.ctx, page.GetFrameTree, cdp.Empty{})
	if err != nil {
		return "", err
	}
	force := true
	r, err := chromedp.Call(b.ctx, css.CreateStyleSheet, css.CreateStyleSheetParams{FrameID: tree.FrameTree.Frame.ID, Force: &force})
	if err != nil {
		return "", err
	}
	_, err = chromedp.Call(b.ctx, css.SetStyleSheetText, css.SetStyleSheetTextParams{StyleSheetID: r.StyleSheetID, Text: rule})
	return r.StyleSheetID, err
}

func (b *Browser) screenshot(clip *page.Viewport) ([]byte, error) {
	if err := b.setTheme(); err != nil {
		return nil, err
	}
	params := page.CaptureScreenshotParams{Format: page.CaptureScreenshotFormatPng, Clip: clip}
	r, err := chromedp.Call(b.ctx, page.CaptureScreenshot, params)
	if err != nil {
		return nil, err
	}
	return r.Data, nil
}

var clientPath = regexp.MustCompile(`^/client/([A-Z0-9]+)(?:/|$)`)

func (b *Browser) workspace(domain string) (string, error) {
	if err := b.navigate("https://" + domain + "/"); err != nil {
		return "", err
	}
	for range 40 {
		loc, err := b.location()
		if err != nil {
			return "", err
		}
		u, e := url.Parse(loc)
		if e == nil && u.Host == "app.slack.com" {
			if match := clientPath.FindStringSubmatch(u.Path); match != nil {
				base := "https://app.slack.com/client/" + match[1]
				if err := b.auth(base, domain); err != nil {
					return "", err
				}
				return base, nil
			}
		}
		if h, _ := b.nodes("h1", 0); len(h) > 0 {
			if text, _ := chromedp.Run(b.ctx, chromedp.Text("h1")); strings.HasPrefix(text, "Sign in to ") {
				return "", fmt.Errorf("authentication required; run slack-auto-screenshot login %s", domain)
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return "", fmt.Errorf("could not resolve workspace domain %s", domain)
}

func (b *Browser) auth(workspace, domain string) error {
	for range 40 {
		loc, err := b.location()
		if err != nil {
			return err
		}
		if !strings.HasPrefix(loc, workspace+"/") && loc != workspace {
			break
		}
		if nodes, _ := b.nodes(`[data-qa="top_nav_search"]`, 0); len(nodes) > 0 {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	if domain == "" {
		domain = "<workspace-domain>"
	}
	return fmt.Errorf("authentication required; run slack-auto-screenshot login %s", domain)
}
