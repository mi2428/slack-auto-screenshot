package app

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"image/png"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/css"
	"github.com/chromedp/chromedp"
)

func TestOptionsAndOutput(t *testing.T) {
	cmd, _, err := NewCommand().Find([]string{"search"})
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.ParseFlags([]string{"yourorg.slack.com", "query", "-C", "5", "-B", "2", "-A", "7"}); err != nil {
		t.Fatal(err)
	}
	if len(cmd.Flags().Args()) != 2 {
		t.Fatal("Cobra must accept flags after operands")
	}
	before, _ := cmd.Flags().GetInt("before")
	after, _ := cmd.Flags().GetInt("after")
	contextCount, _ := cmd.Flags().GetInt("context")
	if before != 2 || after != 7 || contextCount != 5 {
		t.Fatalf("search flags: %d/%d/%d", before, after, contextCount)
	}
	for _, domain := range []string{"https://yourorg.slack.com", "app.slack.com", "evil.slack.com.example.org", "yourorg.slack.com/path"} {
		if validDomain(domain) {
			t.Fatalf("unsafe domain accepted: %q", domain)
		}
	}
	if !validDomain("Amagaeru.Slack.Com") || validDate("2026-02-30") || !validDate("2026-10-02") {
		t.Fatal("domain/date validation")
	}
	for _, args := range [][]string{
		{"capture", "yourorg.slack.com", "#general", "--from", "2026-02-30", "--to", "2026-03-01"},
		{"search", "yourorg.slack.com", "query", "-B", "201"},
		{"capture", "https://evil.test/client/T123/C456"},
	} {
		cmd := NewCommand()
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
	o := Options{Width: 2160, Scale: 2, Output: t.TempDir()}
	if err := validate(&o, "16:9"); err != nil || o.Height != 1215 {
		t.Fatalf("ratio: %+v %v", o, err)
	}
	out, err := NewOutput(o.Output)
	if err != nil {
		t.Fatal(err)
	}
	out.Dates["2026-10-02"], out.Dates["2025-12-04"] = true, true
	if err = os.WriteFile(filepath.Join(out.Stage, "000001.png"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	out.RecordMissing("F123", MissingFile{Timestamp: "42", ID: "F123", Name: "document.md", Reason: "HTTP 403", Source: "https://files.slack.com/files-pri/T-F123/document.md"})
	path, err := out.Finish("../engineering/review", "Channel screenshots: 1\nAttachments: 0 saved, 1 missing", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filepath.Base(path), "engineering_review_2025-12-04_2026-10-02_") {
		t.Fatal(path)
	}
	archive, err := os.Open(path + ".tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	zip, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer zip.Close()
	contents := map[string]string{}
	for tr := tar.NewReader(zip); ; {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		contents[filepath.Base(hdr.Name)] = string(body)
	}
	if contents["000001.png"] != "fixture" || !strings.Contains(contents["MISSING_FILES.txt"], "Missing attachments: 1") || !strings.Contains(contents["MISSING_FILES.txt"], "F123") || !strings.Contains(contents["COMPLETE.txt"], "Captured date range:") || !strings.Contains(contents["COMPLETE.txt"], "1 missing") {
		t.Fatalf("archive contents: %v", contents)
	}
}

func TestCaptureVisibleHistory(t *testing.T) {
	t.Chdir(t.TempDir())
	b, err := OpenBrowser(Options{Width: 800, Height: 600, Scale: 2}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = b.setup(); err != nil {
		t.Fatal(err)
	}
	var html strings.Builder
	html.WriteString(`<style>body{margin:0}#pane{height:500px;overflow-y:auto;pointer-events:none}article{height:110px}</style><input id="search" value="old"><div id="pane" data-qa="slack_kit_scrollbar">`)
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Unix()
	for i := range 30 {
		fmt.Fprintf(&html, `<article data-qa="message_container" data-msg-ts="%d">Post %d</article>`, base+int64(i)*86400, i)
	}
	html.WriteString("</div>")
	if err = b.navigate("data:text/html," + url.PathEscape(html.String())); err != nil {
		t.Fatal(err)
	}
	if err = b.wait(`[data-qa="message_container"]`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err = b.typeInto("#search", "new"); err != nil {
		t.Fatal(err)
	}
	value, err := chromedp.Run(b.ctx, chromedp.Value("#search"))
	if err != nil || value != "new" {
		t.Fatalf("input: %q %v", value, err)
	}
	pane, err := b.paneFor(`[data-qa="message_container"]`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := NewOutput(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	frames, err := b.capturePane(out, pane, out.Stage, frameOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if frames < 6 || len(out.Dates) < 29 {
		t.Fatalf("lost coverage: %d frames, %d dates", frames, len(out.Dates))
	}
	f, err := os.Open(filepath.Join(out.Stage, "000001.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	image, err := png.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	if image.Width != 1600 || image.Height != 1200 {
		t.Fatalf("unexpected pixels: %+v", image)
	}
}

func TestThreadReplyAndSearchContext(t *testing.T) {
	t.Chdir(t.TempDir())
	b, err := OpenBrowser(Options{Width: 800, Height: 600, Scale: 2}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = b.setup(); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC).Unix()
	reply := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC).Unix()
	var html strings.Builder
	html.WriteString(`<style>body{margin:0;--sk_primary_background:26,29,33}[data-qa=slack_kit_scrollbar]{height:240px;overflow-y:auto}article{height:100px}</style>`)
	html.WriteString(`<div data-qa="message_pane"><div data-qa="slack_kit_scrollbar">`)
	for i := range 6 {
		fmt.Fprintf(&html, `<article data-qa="message_container" data-msg-ts="%d">Post`, reply+int64(i)*86400)
		if i == 2 {
			html.WriteString(`<div data-qa="message_kit_files"><a data-qa="message_file_link" href="https://other.slack.com/files/U123/FABC/notes.md?secret=redact">Doc</a><button data-qa="file-collapse-button" aria-label="Toggle 2 attachments">More</button></div>`)
		}
		html.WriteString(`</article>`)
	}
	html.WriteString(`</div></div><div data-qa="threads_flexpane"><div data-qa="slack_kit_scrollbar">`)
	fmt.Fprintf(&html, `<article data-qa="message_container" data-msg-ts="%d">Parent</article><article data-qa="message_container" data-msg-ts="%d">Reply</article>`, old, reply)
	html.WriteString(`</div></div><button data-qa="c-pagination_forward_btn" disabled>Next</button>`)
	if err = b.navigate("data:text/html," + url.PathEscape(html.String())); err != nil {
		t.Fatal(err)
	}
	if err = b.wait(`[data-qa="message_container"]`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	thread, err := b.paneFor(`[data-qa="threads_flexpane"] [data-qa="message_container"]`)
	if err != nil {
		t.Fatal(err)
	}
	match, err := b.scan(thread, "2026-10-02", "2026-10-02")
	if err != nil || !match {
		t.Fatalf("period reply missed: %v %v", match, err)
	}
	out, err := NewOutput(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sheet, err := b.isolateThread()
	if err != nil {
		t.Fatal(err)
	}
	count, err := b.capturePane(out, thread, out.Stage, frameOptions{prefix: "thread_" + fmt.Sprint(old) + "_", thread: true}, nil)
	if err != nil || count == 0 {
		t.Fatalf("thread capture: %d %v", count, err)
	}
	if !out.Dates["2026-09-29"] || !out.Dates["2026-10-02"] {
		t.Fatalf("thread dates: %v", out.Dates)
	}
	_, err = chromedp.Call(b.ctx, css.SetStyleSheetText, css.SetStyleSheetTextParams{StyleSheetID: sheet, Text: ""})
	if err != nil {
		t.Fatal(err)
	}
	if err = b.captureContext(fmt.Sprint(reply+2*86400), filepath.Join(out.Stage, "search"), Options{Before: 1, After: 1}, out); err != nil {
		t.Fatal(err)
	}
	if len(out.Missing) != 2 {
		t.Fatalf("lost attachments: %+v", out.Missing)
	}
	if err = out.WriteMissing(); err != nil {
		t.Fatal(err)
	}
	report, err := os.ReadFile(filepath.Join(out.Stage, "MISSING_FILES.txt"))
	if err != nil || !strings.Contains(string(report), "FABC") || !strings.Contains(string(report), "notes.md") || strings.Contains(string(report), "secret=") {
		t.Fatalf("missing report: %s %v", report, err)
	}
	if ok, err := b.nextPage(""); err != nil || ok {
		t.Fatalf("disabled next page: %v %v", ok, err)
	}
	items, err := os.ReadDir(filepath.Join(out.Stage, "search"))
	if err != nil || len(items) == 0 {
		t.Fatalf("search context: %v %v", items, err)
	}
}

func TestPageOnlyTheme(t *testing.T) {
	t.Chdir(t.TempDir())
	b, err := OpenBrowser(Options{Width: 800, Height: 600, Scale: 1, Theme: "light"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	markup := `<style>.sk-client-theme--dark{--sk_primary_background:26,29,33}.sk-client-theme--light{--sk_primary_background:255,255,255}</style><body class="sk-client-theme--dark">Hello</body>`
	for range 2 {
		if err = b.navigate("data:text/html," + url.PathEscape(markup)); err != nil {
			t.Fatal(err)
		}
		if err = b.setTheme(); err != nil {
			t.Fatal(err)
		}
		body, err := b.first("body", 0)
		if err != nil {
			t.Fatal(err)
		}
		color, err := b.style(body, "--sk_primary_background")
		if err != nil || strings.ReplaceAll(color, " ", "") != "255,255,255" {
			t.Fatalf("theme: %q %v", color, err)
		}
	}
}
