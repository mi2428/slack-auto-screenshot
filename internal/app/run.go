package app

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

var conversationID = regexp.MustCompile(`^[CG][A-Z0-9]+$`)

func Run(o Options) error {
	b, err := OpenBrowser(o, o.Command == "login")
	if err != nil {
		return err
	}
	defer b.Close()
	if o.Command == "login" {
		return login(b, o.Domain)
	}
	if err = b.setup(); err != nil {
		return err
	}
	var workspace, target string
	if o.Domain != "" {
		workspace, err = b.workspace(o.Domain)
		if err != nil {
			return err
		}
	}
	if o.Command == "capture" {
		target = o.Target
		if workspace != "" {
			target, err = b.resolveChannel(workspace, o.Target, o.Domain)
			if err != nil {
				return err
			}
		}
		if err = b.navigate(target); err != nil {
			return err
		}
		if err = b.auth(workspaceFor(target), o.Domain); err != nil {
			return err
		}
		if err = b.dismissAvailabilityReminder(); err != nil {
			return err
		}
		if err = b.closeStaleThreads(); err != nil {
			return err
		}
		if err = b.wait(`[data-qa="message_pane"] [data-qa="message_container"]`, 20*time.Second); err != nil {
			return fmt.Errorf("conversation messages did not appear: %w", err)
		}
		name, err := b.conversationName()
		if err != nil {
			return err
		}
		out, err := NewOutput(o.Output)
		if err != nil {
			return err
		}
		fmt.Println("Capture destination (incomplete):", out.Stage)
		frames, err := b.captureConversation(o, out)
		if err != nil {
			return err
		}
		summary := fmt.Sprintf("Channel screenshots: %d\nAttachments: %d saved, %d missing", frames, len(out.Saved), len(out.Missing))
		path, err := out.Finish(name, summary, o.Gzip)
		if err != nil {
			return err
		}
		if path == "" {
			fmt.Println("No messages matched the capture range")
		} else {
			fmt.Printf("%s\n%s\n", summary, path)
			if o.Gzip {
				fmt.Println("Archive:", path+".tar.gz")
			}
		}
		return nil
	}
	if err = b.openSearch(workspace, o.Query, o.Domain); err != nil {
		return err
	}
	out, err := NewOutput(o.Output)
	if err != nil {
		return err
	}
	fmt.Println("Capture destination (incomplete):", out.Stage)
	count, err := b.captureSearch(workspace, o, out)
	if err != nil {
		return err
	}
	summary := fmt.Sprintf("Search hits: %d\nAttachments: %d saved, %d missing", count, len(out.Saved), len(out.Missing))
	path, err := out.Finish(o.Query, summary, o.Gzip)
	if err != nil {
		return err
	}
	if path == "" {
		fmt.Println("No search hits were captured")
	} else {
		fmt.Printf("%s\n%s\n", summary, path)
		if o.Gzip {
			fmt.Println("Archive:", path+".tar.gz")
		}
	}
	return nil
}

func workspaceFor(conversation string) string {
	parts := strings.Split(strings.TrimPrefix(conversation, "https://app.slack.com/client/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return "https://app.slack.com/client/" + parts[0]
}

func login(b *Browser, domain string) error {
	if err := b.navigate("https://" + domain + "/"); err != nil {
		return err
	}
	fmt.Println("Sign in to Slack in the dedicated Chrome window. Waiting for authentication.")
	stable := 0
	for {
		loc, err := b.location()
		if err != nil {
			return err
		}
		if strings.HasPrefix(loc, "https://app.slack.com/client/") {
			if n, _ := b.nodes(`[data-qa="top_nav_search"]`, 0); len(n) > 0 {
				stable++
			} else {
				stable = 0
			}
		} else {
			stable = 0
		}
		if stable >= 5 {
			fmt.Println("Authentication detected. Closing Chrome.")
			return nil
		}
		select {
		case <-b.ctx.Done():
			return errors.New("authentication aborted: Chrome closed before sign-in completed")
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *Browser) conversationName() (string, error) {
	sel := `[data-qa="channel_name"] .p-view_header__channel_title, [data-qa="dm_title"]`
	nodes, err := b.nodes(sel, 0)
	if err != nil {
		return "", err
	}
	var labels []string
	for _, n := range nodes {
		if _, e := b.box(n); e != nil {
			continue
		}
		t, e := b.text(n)
		if e == nil && strings.TrimSpace(t) != "" {
			labels = append(labels, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(t), "#")))
		}
	}
	if len(labels) != 1 {
		return "", errors.New("could not identify a unique conversation title")
	}
	return labels[0], nil
}

func (b *Browser) closeStaleThreads() error {
	if err := b.dismissAutocomplete(); err != nil {
		return err
	}
	for range 5 {
		panes, e := b.nodes(`[data-qa="threads_flexpane"]`, 0)
		if e != nil {
			return e
		}
		if len(panes) == 0 {
			return nil
		}
		var visible cdp.NodeID
		for _, pane := range panes {
			if _, e := b.box(pane); e == nil {
				visible = pane
			}
		}
		if visible == 0 {
			return nil
		}
		buttons, e := b.nodes(`[data-qa="close_flexpane"], [data-qa="unstyled-button"][aria-label="Close"]`, visible)
		if e != nil || len(buttons) == 0 {
			return errors.New("could not close a previously opened thread pane")
		}
		if e = b.clickID(buttons[0]); e != nil {
			return e
		}
		if e = b.waitThreadClosed(visible); e != nil {
			return e
		}
	}
	return errors.New("thread pane remains open")
}

func (b *Browser) waitThreadClosed(pane cdp.NodeID) error {
	for range 40 {
		if _, err := b.box(pane); err != nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("thread pane did not close")
}

func (b *Browser) dismissAutocomplete() error {
	modal, err := b.nodes(`.c-search_modal--autocomplete`, 0)
	if err != nil {
		return err
	}
	if len(modal) == 0 {
		return nil
	}
	if _, err = b.box(modal[0]); err != nil {
		return nil
	}
	for _, typ := range []input.DispatchKeyEventType{input.DispatchKeyEventTypeRawKeyDown, input.DispatchKeyEventTypeKeyUp} {
		if _, err = chromedp.Call(b.ctx, input.DispatchKeyEvent, input.DispatchKeyEventParams{Type: typ, Key: "Escape", Code: "Escape", WindowsVirtualKeyCode: 27}); err != nil {
			return err
		}
	}
	return nil
}

func (b *Browser) dismissAvailabilityReminder() error {
	buttons, err := b.nodes(`[data-qa="sk_close_modal_button"]`, 0)
	if err != nil || len(buttons) == 0 {
		return err
	}
	if _, err = b.box(buttons[0]); err != nil {
		return nil
	}
	dialogs, err := b.nodes(`[role="dialog"]:has([data-qa="sk_close_modal_button"])`, 0)
	if err != nil {
		return err
	}
	if len(dialogs) != 1 {
		return errors.New("unexpected Slack dialog is open")
	}
	text, err := b.text(dialogs[0])
	if err != nil {
		return err
	}
	if !strings.Contains(text, "Set yourself to active?") {
		return errors.New("unexpected Slack dialog is open")
	}
	return b.clickID(buttons[0])
}

func (b *Browser) resolveChannel(workspace, name, domain string) (string, error) {
	if err := b.navigate(workspace); err != nil {
		return "", err
	}
	if err := b.auth(workspace, domain); err != nil {
		return "", err
	}
	if err := b.dismissAvailabilityReminder(); err != nil {
		return "", err
	}
	input := `[data-qa="focusable_search_input"] [data-qa="texty_input"]`
	if n, _ := b.nodes(input, 0); len(n) == 0 {
		if err := b.click(`[data-qa="top_nav_search"]`); err != nil {
			return "", err
		}
	}
	if err := b.typeInto(input, strings.TrimPrefix(name, "#")); err != nil {
		return "", err
	}
	for range 75 {
		matches, e := b.nodes(`[role="option"]:has([data-qa="small_channel_entity"])`, 0)
		if e != nil {
			return "", e
		}
		var ids []string
		for _, m := range matches {
			children, e := b.nodes(`[data-qa="small_channel_entity"]`, m)
			if e != nil || len(children) != 1 {
				continue
			}
			label, e := b.text(children[0])
			if e != nil || strings.TrimPrefix(strings.TrimSpace(label), "#") != strings.TrimPrefix(name, "#") {
				continue
			}
			id, e := b.attr(m, "data-id")
			if e == nil {
				ids = append(ids, id)
			}
		}
		if len(ids) > 1 {
			return "", errors.New("multiple channels have this name; use a conversation URL")
		}
		if len(ids) == 1 {
			if !conversationID.MatchString(ids[0]) {
				return "", errors.New("invalid channel ID")
			}
			return workspace + "/" + ids[0], nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return "", fmt.Errorf("channel %s not found", name)
}

func (b *Browser) text(id cdp.NodeID) (string, error) {
	return chromedp.Run(b.ctx, chromedp.Text([]cdp.NodeID{id}))
}
func (b *Browser) clickID(id cdp.NodeID) error {
	r, err := b.box(id)
	if err != nil {
		return err
	}
	x, y := r.x1+r.width()/2, r.y1+r.height()/2
	if err = b.hoverID(id); err != nil {
		return err
	}
	for _, typ := range []input.DispatchMouseEventType{input.DispatchMouseEventTypeMousePressed, input.DispatchMouseEventTypeMouseReleased} {
		_, err = chromedp.Call(b.ctx, input.DispatchMouseEvent, input.DispatchMouseEventParams{Type: typ, X: x, Y: y, Button: input.MouseButtonLeft, ClickCount: 1})
		if err != nil {
			return err
		}
	}
	return nil
}

func (b *Browser) hoverID(id cdp.NodeID) error {
	r, err := b.box(id)
	if err != nil {
		return err
	}
	_, err = chromedp.Call(b.ctx, input.DispatchMouseEvent, input.DispatchMouseEventParams{Type: input.DispatchMouseEventTypeMouseMoved, X: r.x1 + r.width()/2, Y: r.y1 + r.height()/2, Button: input.MouseButtonNone})
	if err == nil {
		time.Sleep(80 * time.Millisecond)
	}
	return err
}
