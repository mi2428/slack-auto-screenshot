package app

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

var directFile = regexp.MustCompile(`^/files-pri/[A-Z0-9]+-(F[A-Z0-9]+)/([^/]+)$`)
var fileID = regexp.MustCompile(`(?:/|-)(F[A-Z0-9]+)(?:/|$)`)
var attachmentCount = regexp.MustCompile(`^Toggle (\d+) attachments$`)

// Only the rendered message's attachment block is considered; ordinary links
// in the message body must not be downloaded or allowed to send session cookies.
func (b *Browser) collectFiles(out *Output, messages []message) error {
	for _, msg := range messages {
		groups, err := b.nodes(`[data-qa="message_kit_files"]`, msg.id)
		if err != nil {
			return err
		}
		for _, group := range groups {
			links, err := b.nodes(`a[href*="files.slack.com/files-pri/"],a[data-qa="message_file_link"]`, group)
			if err != nil {
				return err
			}
			name := "(unknown name)"
			if n, e := b.nodes(`[data-qa="file_name"]`, group); e == nil && len(n) > 0 {
				if text, e := b.text(n[0]); e == nil && strings.TrimSpace(text) != "" {
					name = strings.TrimSpace(text)
				}
			}
			for _, link := range links {
				href, err := b.attr(link, "href")
				if err != nil {
					return err
				}
				linkName := name
				if labels, e := b.nodes(`[data-qa="file_name"]`, link); e == nil && len(labels) > 0 {
					if text, e := b.text(labels[0]); e == nil && strings.TrimSpace(text) != "" {
						linkName = strings.TrimSpace(text)
					}
				} else if u, e := url.Parse(href); e == nil && filepath.Base(u.Path) != "." {
					linkName = filepath.Base(u.Path)
				}
				if err = b.download(out, msg.ts, linkName, href); err != nil {
					return err
				}
			}
			count := 1
			if buttons, e := b.nodes(`[data-qa="file-collapse-button"]`, group); e == nil && len(buttons) > 0 {
				if label, e := b.attr(buttons[0], "aria-label"); e == nil {
					if match := attachmentCount.FindStringSubmatch(label); len(match) > 1 {
						count, _ = strconv.Atoi(match[1])
					}
				}
			}
			for i := len(links); i < count; i++ {
				missingName := fmt.Sprintf("%s (%d/%d)", name, i+1, count)
				out.RecordMissing(msg.ts+":"+missingName, MissingFile{Timestamp: msg.ts, ID: "(unknown)", Name: missingName, Reason: "original file URL unavailable", Source: "(no link)"})
			}
		}
	}
	return nil
}

func (b *Browser) download(out *Output, ts, name, href string) error {
	u, err := url.Parse(href)
	if err != nil || u.Scheme != "https" || u.User != nil || !strings.HasSuffix(u.Hostname(), ".slack.com") {
		out.RecordMissing(ts+":"+href, MissingFile{Timestamp: ts, ID: "(unknown)", Name: name, Reason: "invalid attachment URL", Source: "(invalid link)"})
		return nil
	}
	id := fileID.FindStringSubmatch(u.EscapedPath())
	key := ts + ":" + u.Path
	if len(id) > 1 {
		key = id[1]
	}
	if out.Saved[key] || out.Missing[key].Reason != "" {
		return nil
	}
	source := u.Scheme + "://" + u.Host + u.EscapedPath()
	missing := func(reason string) {
		unknown := "(unknown)"
		if len(id) > 1 {
			unknown = id[1]
		}
		out.RecordMissing(key, MissingFile{Timestamp: ts, ID: unknown, Name: name, Reason: reason, Source: source})
	}
	match := directFile.FindStringSubmatch(u.EscapedPath())
	if u.Host != "files.slack.com" || len(match) != 3 {
		missing("original file URL unavailable")
		return nil
	}
	if len(id) <= 1 || id[1] != match[1] {
		missing("invalid file ID")
		return nil
	}
	filename, err := url.PathUnescape(match[2])
	if err != nil {
		missing("file name unavailable")
		return nil
	}
	filename = safeName(filename, 120)
	if filename == "" {
		missing("file name unavailable")
		return nil
	}
	cookies, err := chromedp.Call(b.ctx, network.GetCookies, network.GetCookiesParams{URLs: []string{u.String()}})
	if err != nil {
		missing("authentication cookies unavailable")
		return nil
	}
	req, err := http.NewRequestWithContext(b.ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		missing("could not create attachment request")
		return nil
	}
	for _, cookie := range cookies.Cookies {
		req.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	}
	client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		missing("network request failed")
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 || strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") && !strings.HasSuffix(strings.ToLower(filename), ".html") {
		missing(fmt.Sprintf("download failed (HTTP %d)", resp.StatusCode))
		return nil
	}
	target := filepath.Join(out.Stage, "file_"+match[1]+"_"+filename)
	tmp, err := os.CreateTemp(out.Stage, ".file-*.partial")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	guard := &fileWriter{file: tmp}
	if _, err = io.Copy(guard, resp.Body); err != nil {
		_ = tmp.Close()
		if guard.err != nil {
			return guard.err
		}
		missing("download interrupted")
		return nil
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if _, err = os.Stat(target); err == nil {
		return fmt.Errorf("attachment file name collision: %s", target)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = os.Rename(tmp.Name(), target); err != nil {
		return err
	}
	out.Saved[key] = true
	fmt.Printf("Attachment saved: %s (%d total)\n", filepath.Base(target), len(out.Saved))
	return nil
}

type fileWriter struct {
	file *os.File
	err  error
}

func (w *fileWriter) Write(p []byte) (int, error) { n, e := w.file.Write(p); w.err = e; return n, e }
