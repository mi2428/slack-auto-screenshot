package app

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

var conversationID = regexp.MustCompile(`^[CG][A-Z0-9]+$`)

func Run(o Options) error {
	if o.Command == "search" {
		return runSearch(o)
	}
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
			if len(out.Gaps) > 0 {
				fmt.Fprintln(os.Stderr, "WARNING: capture skipped a possible matching range; no screenshots were saved")
			} else {
				fmt.Println("No messages matched the capture range")
			}
		} else {
			fmt.Printf("%s\n%s\n", summary, path)
			if len(out.Gaps) > 0 {
				fmt.Fprintf(os.Stderr, "WARNING: %d possible screenshot gap(s); see COMPLETE.txt in %s\n", len(out.Gaps), path)
			}
			if o.Gzip {
				fmt.Println("Archive:", path+".tar.gz")
			}
		}
		return nil
	}
	return errors.New("unknown command")
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
			if n, _ := b.nodes(slackAppReady, 0); len(n) > 0 {
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
		if e = b.waitThreadClosed(); e != nil {
			return e
		}
	}
	return errors.New("thread pane remains open")
}

func (b *Browser) waitThreadClosed() error {
	for range 40 {
		panes, err := b.nodes(`[data-qa="threads_flexpane"]`, 0)
		if err != nil {
			return err
		}
		visible := false
		for _, pane := range panes {
			if _, err := b.box(pane); err == nil {
				visible = true
				break
			}
		}
		if !visible {
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
	dialogs, err := b.nodes(`[role="dialog"]`, 0)
	if err != nil {
		return err
	}
	var dialog cdp.NodeID
	for _, id := range dialogs {
		if _, err = b.box(id); err != nil {
			continue
		}
		if dialog != 0 {
			return errors.New("multiple Slack dialogs are open")
		}
		dialog = id
	}
	if dialog == 0 {
		return nil
	}
	text, err := b.text(dialog)
	if err != nil {
		return err
	}
	if !strings.Contains(text, "Set yourself to active?") {
		return errors.New("unexpected Slack dialog is open")
	}
	buttons, err := b.nodes(`button, [role="button"]`, dialog)
	if err != nil {
		return err
	}
	var choice cdp.NodeID
	for _, id := range buttons {
		if _, err = b.box(id); err != nil {
			continue
		}
		label, err := b.text(id)
		if err != nil {
			return err
		}
		if strings.TrimSpace(label) == "" {
			label, err = b.attr(id, "aria-label")
			if err != nil {
				return err
			}
		}
		if isCurrentStatusChoice(label) {
			if choice != 0 {
				return errors.New("multiple current-status choices are available")
			}
			choice = id
		}
	}
	if choice == 0 {
		return errors.New("Slack availability reminder has no safe current-status choice")
	}
	if err = b.clickID(choice); err != nil {
		return err
	}
	for range 20 {
		visible := false
		dialogs, err = b.nodes(`[role="dialog"]`, 0)
		if err != nil {
			return err
		}
		for _, id := range dialogs {
			if _, err = b.box(id); err == nil {
				visible = true
				break
			}
		}
		if !visible {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("Slack availability reminder remained open after choosing current status")
}

func isCurrentStatusChoice(label string) bool {
	label = strings.ToLower(strings.Join(strings.Fields(label), " "))
	return label == "no" || label == "not now" || strings.HasPrefix(label, "no, keep ") ||
		strings.HasPrefix(label, "keep my status") || strings.HasPrefix(label, "keep current status") ||
		strings.HasPrefix(label, "keep me away")
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
	if _, err := chromedp.Call(b.ctx, dom.ScrollIntoViewIfNeeded, dom.ScrollIntoViewIfNeededParams{NodeID: id}); err != nil {
		return fmt.Errorf("scroll click target node %d: %w", id, err)
	}
	if _, err := b.box(id); err != nil {
		return err
	}
	resolved, err := chromedp.Call(b.ctx, dom.ResolveNode, dom.ResolveNodeParams{NodeID: id})
	if err != nil {
		return fmt.Errorf("resolve click target node %d: %w", id, err)
	}
	if resolved.Object == nil || resolved.Object.ObjectID == "" {
		return fmt.Errorf("resolve click target node %d: no JavaScript object", id)
	}
	userGesture := true
	result, err := chromedp.Call(b.ctx, runtime.CallFunctionOn, runtime.CallFunctionOnParams{
		FunctionDeclaration: `function(){this.click()}`,
		ObjectID:            resolved.Object.ObjectID,
		UserGesture:         &userGesture,
	})
	if err != nil {
		return err
	}
	if result.ExceptionDetails != nil {
		return errors.New("click target raised a JavaScript exception")
	}
	return nil
}
