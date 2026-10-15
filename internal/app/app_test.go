package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/css"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/chromedp"
)

func TestOptionsAndOutput(t *testing.T) {
	cmd, _, err := NewCommand().Find([]string{"search"})
	if err != nil {
		t.Fatal(err)
	}
	scale, _ := cmd.Flags().GetFloat64("scale")
	width, _ := cmd.Flags().GetInt("width")
	outputWidth, _ := cmd.Flags().GetInt("output-width")
	if scale != 1.5 || width != 1440 || outputWidth != 1440 {
		t.Fatalf("unexpected viewport defaults: %d CSS px, scale %g, %d output px", width, scale, outputWidth)
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

func TestThreadDateScanOnlyForParentsOutsideRange(t *testing.T) {
	for _, tc := range []struct {
		date string
		want bool
	}{
		{"2023-02-28", true},
		{"2023-03-01", false},
		{"2026-10-15", false},
		{"2026-10-16", true},
	} {
		date, err := time.ParseInLocation("2006-01-02", tc.date, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		got, err := needsThreadDateScan(fmt.Sprintf("%d.000001", date.Add(12*time.Hour).Unix()), "2023-03-01", "2026-10-15")
		if err != nil || got != tc.want {
			t.Fatalf("parent %s: needs scan %v, want %v: %v", tc.date, got, tc.want, err)
		}
	}
	if _, err := needsThreadDateScan("invalid", "2023-03-01", "2026-10-15"); err == nil {
		t.Fatal("invalid parent timestamp accepted")
	}
}

func TestWheelMovesViewportFractionAcrossScales(t *testing.T) {
	for _, scale := range []float64{1.5, 2} {
		t.Run(fmt.Sprintf("scale%g", scale), func(t *testing.T) {
			t.Chdir(t.TempDir())
			b, err := OpenBrowser(Options{Width: 1440, Height: 2560, Scale: scale, OutputWidth: 1440}, false)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			if err = b.setup(); err != nil {
				t.Fatal(err)
			}
			var html strings.Builder
			html.WriteString(`<style>body{margin:0;overflow:hidden}#pane{height:2325px;overflow-y:auto}article{height:500px}</style><div id="pane">`)
			base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Unix()
			for i := range 30 {
				fmt.Fprintf(&html, `<article data-qa="message_container" data-msg-ts="%d">Post %d</article>`, base+int64(i)*86400, i)
			}
			html.WriteString(`</div>`)
			if err = b.navigate("data:text/html," + url.PathEscape(html.String())); err != nil {
				t.Fatal(err)
			}
			pane, err := b.first("#pane", 0)
			if err != nil {
				t.Fatal(err)
			}
			rect, err := b.box(pane)
			if err != nil {
				t.Fatal(err)
			}
			top := 0.0
			// Do not install wheel listeners: they can change Chrome's input routing.
			for _, direction := range []float64{1, 1, -1} {
				if _, err = b.screenshot(nil); err != nil {
					t.Fatal(err)
				}
				delta := rect.height() * 0.95 * direction
				if err = b.wheel(pane, delta); err != nil {
					t.Fatal(err)
				}
				after, err := chromedp.Run(b.ctx, chromedp.Evaluate[float64](`document.getElementById('pane').scrollTop`))
				if err != nil {
					t.Fatal(err)
				}
				if math.Abs(after-top-delta) > 1 {
					t.Fatalf("wheel moved %.2f CSS px, want %.2f (95%% of pane)", after-top, delta)
				}
				top = after
			}
		})
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
	html.WriteString(`<style>body{margin:0}#pane{height:500px;overflow-y:auto}article{height:110px}</style><input id="search" value="old"><div id="pane" data-qa="slack_kit_scrollbar">`)
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
	jumped := false
	frames, err := b.capturePane(out, pane, out.Stage, frameOptions{}, func(pane cdp.NodeID) error {
		if jumped {
			return nil
		}
		jumped = true
		return b.wheel(pane, float64(b.height)*2)
	})
	if err != nil {
		t.Fatal(err)
	}
	if frames < 6 || len(out.Dates) != 30 || len(out.Gaps) != 0 {
		t.Fatalf("lost coverage: %d frames, %d dates, %d gaps", frames, len(out.Dates), len(out.Gaps))
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
	bestEffort, err := NewOutput(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jumped = false
	_, err = b.capturePane(bestEffort, pane, bestEffort.Stage, frameOptions{}, func(pane cdp.NodeID) error {
		if jumped {
			return nil
		}
		jumped = true
		if err := b.wheel(pane, float64(b.height)*2); err != nil {
			return err
		}
		// Remove earlier history so native backward scrolling cannot restore it.
		for i := range 10 {
			anchor, err := b.first(fmt.Sprintf(`[data-qa="message_container"][data-msg-ts="%d"]`, base+int64(i)*86400), pane)
			if err != nil {
				return err
			}
			if _, err = chromedp.Call(b.ctx, dom.RemoveNode, dom.RemoveNodeParams{NodeID: anchor}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || !bestEffort.Dates["2026-10-30"] || len(bestEffort.Gaps) != 1 {
		t.Fatalf("best effort did not continue with a gap warning: %v %v %v", bestEffort.Dates, bestEffort.Gaps, err)
	}
	path, err := bestEffort.Finish("fixture", "Channel screenshots: 1", false)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := os.ReadFile(filepath.Join(path, "COMPLETE.txt"))
	if err != nil || !strings.Contains(string(marker), "WARNING: 1 possible screenshot gap(s)") || !strings.Contains(string(marker), bestEffort.Gaps[0]) {
		t.Fatalf("missing gap warning in completion marker: %s %v", marker, err)
	}
}

func TestCaptureSidebarReflowsAroundThreads(t *testing.T) {
	t.Chdir(t.TempDir())
	b, err := OpenBrowser(Options{Width: 800, Height: 600, Scale: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = b.setup(); err != nil {
		t.Fatal(err)
	}
	html := `<style>body{margin:0}.p-client_workspace__tabpanel{display:grid;grid-template-columns:200px minmax(0,1fr);width:800px}.p-view_contents--primary{grid-column:2;min-width:0}</style><div class="p-client_workspace__tabpanel"><div class="p-view_contents--sidebar">Channels</div><div class="p-view_contents--primary" data-qa="message_pane">Message</div><div id="thread">Thread</div></div>`
	if err = b.navigate("data:text/html," + url.PathEscape(html)); err != nil {
		t.Fatal(err)
	}
	pane, err := b.first(`[data-qa="message_pane"]`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.styleSheet(captureSidebarCSS); err != nil {
		t.Fatal(err)
	}
	expanded, err := b.box(pane)
	if err != nil || expanded.x1 != 0 || expanded.width() != 800 {
		t.Fatalf("message pane did not expand: %+v %v", expanded, err)
	}
	thread, err := b.first("#thread", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = chromedp.Call(b.ctx, dom.SetAttributeValue, dom.SetAttributeValueParams{NodeID: thread, Name: "data-qa", Value: "threads_flexpane"}); err != nil {
		t.Fatal(err)
	}
	withThread, err := b.box(pane)
	if err != nil || withThread.x1 != 200 || withThread.width() != 600 {
		t.Fatalf("thread pane did not restore native grid: %+v %v", withThread, err)
	}
	if _, err = chromedp.Call(b.ctx, dom.SetAttributeValue, dom.SetAttributeValueParams{NodeID: thread, Name: "data-qa", Value: "closed"}); err != nil {
		t.Fatal(err)
	}
	again, err := b.box(pane)
	if err != nil || again.x1 != 0 || again.width() != 800 {
		t.Fatalf("message pane did not expand after thread close: %+v %v", again, err)
	}
}

func TestWQHDPortraitScale(t *testing.T) {
	t.Chdir(t.TempDir())
	cmd, _, err := NewCommand().Find([]string{"capture"})
	if err != nil {
		t.Fatal(err)
	}
	width, _ := cmd.Flags().GetInt("width")
	scale, _ := cmd.Flags().GetFloat64("scale")
	outputWidth, _ := cmd.Flags().GetInt("output-width")
	if width != 1440 || scale != 1.5 || outputWidth != 1440 {
		t.Fatalf("viewport flags: %d, %.2f", width, scale)
	}
	for _, invalid := range []float64{math.NaN(), math.Inf(1)} {
		o := Options{Width: 1440, Scale: invalid, Output: t.TempDir()}
		if validate(&o, "9:16") == nil {
			t.Fatalf("invalid scale accepted: %v", invalid)
		}
	}
	o := Options{Width: width, Scale: scale, OutputWidth: outputWidth, Output: t.TempDir()}
	if err = validate(&o, "9:16"); err != nil || o.Height != 2560 {
		t.Fatalf("portrait viewport: %+v %v", o, err)
	}
	b, err := OpenBrowser(o, false)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = b.setup(); err != nil {
		t.Fatal(err)
	}
	if err = b.navigate("data:text/html,<body>WQHD</body>"); err != nil {
		t.Fatal(err)
	}
	data, err := b.screenshot(nil)
	if err != nil {
		t.Fatal(err)
	}
	image, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || image.Width != 1440 || image.Height != 2560 {
		t.Fatalf("WQHD PNG: %+v %v", image, err)
	}
}

func TestScreenshotWaitsForAttachmentImage(t *testing.T) {
	t.Chdir(t.TempDir())
	b, err := OpenBrowser(Options{Width: 800, Height: 600, Scale: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = b.setup(); err != nil {
		t.Fatal(err)
	}
	image := image.NewRGBA(image.Rect(0, 0, 1, 1))
	image.Set(0, 0, color.RGBA{R: 200, A: 255})
	var contents bytes.Buffer
	if err = png.Encode(&contents, image); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}, 1), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			_, _ = io.WriteString(w, `<body style="margin:0"><div data-qa="message_kit_files"><img id="preview" style="width:100px;height:100px"></div></body>`)
			return
		}
		if r.URL.Path != "/image" {
			http.NotFound(w, r)
			return
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(contents.Bytes())
	}))
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		server.Close()
	}()
	if err = b.navigate(server.URL); err != nil {
		t.Fatal(err)
	}
	if _, err = chromedp.Run(b.ctx, chromedp.Evaluate[chromedp.Void](`document.getElementById('preview').src=`+strconv.Quote(server.URL+"/image"))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("attachment image request did not start")
	}
	pending, err := chromedp.Run(b.ctx, chromedp.Evaluate[int](`Array.from(document.querySelectorAll('[data-qa="message_kit_files"] img')).filter(i=>{const r=i.getBoundingClientRect();return r.width>0&&r.height>0&&r.top<innerHeight&&r.bottom>0&&r.left<innerWidth&&r.right>0&&!i.complete}).length`))
	if err != nil || pending != 1 {
		t.Fatalf("expected pending file image before screenshot: %d %v", pending, err)
	}
	result := make(chan struct {
		data []byte
		err  error
	}, 1)
	go func() {
		data, err := b.screenshot(nil)
		result <- struct {
			data []byte
			err  error
		}{data, err}
	}()
	select {
	case <-result:
		t.Fatal("captured incomplete attachment image")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	shot := <-result
	if shot.err != nil {
		t.Fatal(shot.err)
	}
	decoded, err := png.Decode(bytes.NewReader(shot.data))
	if err != nil {
		t.Fatal(err)
	}
	r, g, blue, _ := decoded.At(50, 50).RGBA()
	if r < 0xc000 || g > 0x2000 || blue > 0x2000 {
		t.Fatalf("preview was not rendered: %04x %04x %04x", r, g, blue)
	}
}

func TestWideCSSViewportScaledScreenshot(t *testing.T) {
	t.Chdir(t.TempDir())
	b, err := OpenBrowser(Options{Width: 2160, Height: 3840, Scale: 1.5, OutputWidth: 1440}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = b.setup(); err != nil {
		t.Fatal(err)
	}
	if err = b.navigate("data:text/html,<body>Wide layout</body>"); err != nil {
		t.Fatal(err)
	}
	data, err := b.screenshot(nil)
	if err != nil {
		t.Fatal(err)
	}
	image, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if image.Width != 1440 || image.Height != 2560 {
		t.Fatalf("WQHD image from wide CSS layout: %dx%d", image.Width, image.Height)
	}
}

func TestThreadReplyAndSearchContext(t *testing.T) {
	t.Chdir(t.TempDir())
	b, err := OpenBrowser(Options{Width: 800, Height: 600, Scale: 1.5, OutputWidth: 600}, false)
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

func TestWaitThreadClosedAfterReplacement(t *testing.T) {
	t.Chdir(t.TempDir())
	b, err := OpenBrowser(Options{Width: 800, Height: 600, Scale: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = b.navigate("data:text/html," + url.PathEscape(`<div id="old" data-qa="threads_flexpane">Old</div><div id="new" data-qa="threads_flexpane">Still open</div>`)); err != nil {
		t.Fatal(err)
	}
	old, err := b.first("#old", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = chromedp.Call(b.ctx, dom.RemoveNode, dom.RemoveNodeParams{NodeID: old}); err != nil {
		t.Fatal(err)
	}
	if err = b.waitThreadClosed(); err == nil {
		t.Fatal("a replacement thread pane is still visible")
	}
	fresh, err := b.first("#new", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = chromedp.Call(b.ctx, dom.SetAttributeValue, dom.SetAttributeValueParams{NodeID: fresh, Name: "style", Value: "display:none"}); err != nil {
		t.Fatal(err)
	}
	if err = b.waitThreadClosed(); err != nil {
		t.Fatal(err)
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
