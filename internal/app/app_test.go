package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
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

func writeTestPNG(t *testing.T, path string, width, height int) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = png.Encode(file, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
}

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
	if err = cmd.ParseFlags([]string{"yourorg.slack.com", "query", "--channel-messages-only"}); err != nil {
		t.Fatal(err)
	}
	channelMessagesOnly, _ := cmd.Flags().GetBool("channel-messages-only")
	if !channelMessagesOnly {
		t.Fatal("--channel-messages-only was not enabled")
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
	if err = os.Link(filepath.Join(out.Stage, "000001.png"), filepath.Join(out.Stage, "linked.png")); err != nil {
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
	defer func() {
		if err := archive.Close(); err != nil {
			t.Error(err)
		}
	}()
	zip, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := zip.Close(); err != nil {
			t.Error(err)
		}
	}()
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
	if contents["000001.png"] != "fixture" || contents["linked.png"] != "fixture" || !strings.Contains(contents["MISSING_FILES.txt"], "Missing attachments: 1") || !strings.Contains(contents["MISSING_FILES.txt"], "F123") || !strings.Contains(contents["COMPLETE.txt"], "Captured date range:") || !strings.Contains(contents["COMPLETE.txt"], "1 missing") {
		t.Fatalf("archive contents: %v", contents)
	}
}

func TestFinishKeepsStageAfterArchiveFailure(t *testing.T) {
	out, err := NewOutput(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out.Dates["2026-10-02"] = true
	if err = os.WriteFile(filepath.Join(out.Stage, "000001.png"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(out.Stage, "special")
	if err = os.Symlink("000001.png", bad); err != nil {
		t.Fatal(err)
	}
	if _, err = out.Finish("fixture", "summary", true); err == nil {
		t.Fatal("archive accepted a special file")
	}
	if info, statErr := os.Stat(out.Stage); statErr != nil || !info.IsDir() {
		t.Fatalf("stage was lost after archive failure: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(out.Stage, "COMPLETE.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("failed output retained a completion marker: %v", statErr)
	}
	if err = os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	if _, err = out.Finish("fixture", "summary", true); err != nil {
		t.Fatalf("archive retry failed: %v", err)
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

func TestAuthHasDeadline(t *testing.T) {
	t.Chdir(t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<html></html>")
	}))
	defer server.Close()
	b, err := OpenBrowser(Options{Width: 800, Height: 600, Scale: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = b.navigate(server.URL); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = b.authWithin(server.URL, "", 50*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("auth deadline: elapsed=%s err=%v", time.Since(started), err)
	}
}

func TestSearchContextHasDeadline(t *testing.T) {
	t.Chdir(t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		_, _ = io.WriteString(w, `<style>#pane{height:500px;overflow-y:auto}article{height:200px}</style><div data-qa="message_pane"><div id="pane" data-qa="slack_kit_scrollbar"><article data-qa="message_container" data-msg-ts="1730000000.000000"><div data-qa="message_kit_files"><img id="slow" style="width:100px;height:100px"></div></article></div></div>`)
	}))
	defer server.Close()
	b, err := OpenBrowser(Options{Width: 800, Height: 600, Scale: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = b.navigate(server.URL); err != nil {
		t.Fatal(err)
	}
	if _, err = chromedp.Run(b.ctx, chromedp.Evaluate[chromedp.Void](`document.getElementById('slow').src=`+strconv.Quote(server.URL+"/slow"))); err != nil {
		t.Fatal(err)
	}
	out := &Output{Stage: t.TempDir(), Dates: map[string]bool{}, Saved: map[string]bool{}, Missing: map[string]MissingFile{}}
	started := time.Now()
	err = b.captureContextWithin("1730000000.000000", "1730000000.000000", filepath.Join(out.Stage, "hit"), Options{}, out, false, nil, 50*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("search context deadline: elapsed=%s err=%v", time.Since(started), err)
	}
}

func TestWheelWaitsForDelayedVisibleMessages(t *testing.T) {
	t.Chdir(t.TempDir())
	b, err := OpenBrowser(Options{Width: 800, Height: 600, Scale: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = b.setup(); err != nil {
		t.Fatal(err)
	}
	markup := `<style>body{margin:0}#pane{height:500px;overflow-y:auto}article{height:100px;position:sticky;top:0}#space{height:4000px}</style>` +
		`<div id="pane"><article data-qa="message_container" data-msg-ts="1730000000.000000">Post</article><div id="space"></div></div>` +
		`<script>pane.addEventListener('scroll',()=>{setTimeout(()=>document.querySelector('article').dataset.msgTs='1740000000.000000',150);setTimeout(()=>document.querySelector('article').dataset.msgTs='1750000000.000000',240)},{once:true})</script>`
	if err = b.navigate("data:text/html," + url.PathEscape(markup)); err != nil {
		t.Fatal(err)
	}
	pane, err := b.first("#pane", 0)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err = b.wheel(pane, 400); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	messages, err := b.visible(pane, nil)
	if err != nil || len(messages) != 1 || messages[0].ts != "1750000000.000000" || elapsed < 375*time.Millisecond || elapsed > 700*time.Millisecond {
		t.Fatalf("adaptive wheel wait: elapsed=%s messages=%+v err=%v", elapsed, messages, err)
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
	defer func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}()
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
	fmt.Fprintf(&html, `<article data-qa="message_container" data-msg-ts="%d">Parent</article>`, old)
	for i := range 6 {
		fmt.Fprintf(&html, `<article data-qa="message_container" data-msg-ts="%d">Post`, reply+int64(i)*86400)
		if i == 2 {
			html.WriteString(`<div data-qa="message_kit_files"><a data-qa="message_file_link" href="https://other.slack.com/files/U123/FABC/notes.md?secret=redact">Doc</a><button data-qa="file-collapse-button" aria-label="Toggle 2 attachments">More</button></div>`)
		}
		html.WriteString(`</article>`)
	}
	html.WriteString(`</div></div><div data-qa="threads_flexpane"><div data-qa="slack_kit_scrollbar">`)
	fmt.Fprintf(&html, `<article data-qa="message_container" data-msg-ts="%d">Parent</article><article data-qa="message_container" data-msg-ts="%d">Reply</article><article data-qa="message_container" data-msg-ts="%d">Second reply</article>`, old, reply, reply+86400)
	html.WriteString(`</div></div>`)
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
	channelHit := fmt.Sprint(reply + 2*86400)
	if err = b.captureContext(channelHit, channelHit, filepath.Join(out.Stage, "search"), Options{Before: 1, After: 1}, out, false, nil); err != nil {
		t.Fatal(err)
	}
	nextChannelHit := fmt.Sprint(reply + 3*86400)
	if err = b.captureContext(nextChannelHit, nextChannelHit, filepath.Join(out.Stage, "channel-reused"), Options{Before: 1, After: 1}, out, true, nil); err != nil {
		t.Fatal(err)
	}
	if err = b.captureContext(fmt.Sprint(reply), fmt.Sprint(old), filepath.Join(out.Stage, "split"), Options{}, out, false, nil); err != nil {
		t.Fatal(err)
	}
	if err = b.captureContext(fmt.Sprint(reply+86400), fmt.Sprint(old), filepath.Join(out.Stage, "reused"), Options{}, out, true, nil); err != nil {
		t.Fatal(err)
	}
	broadcast := `<style>body{margin:0}[data-qa=slack_kit_scrollbar]{height:240px;overflow-y:auto}article{height:100px}</style>` +
		`<div data-qa="message_pane"><div data-qa="slack_kit_scrollbar"><article data-qa="message_container" data-msg-ts="` + fmt.Sprint(reply) + `">Broadcast reply</article></div></div>`
	if err = b.navigate("data:text/html," + url.PathEscape(broadcast)); err != nil {
		t.Fatal(err)
	}
	if err = b.captureContext(fmt.Sprint(reply), fmt.Sprint(old), filepath.Join(out.Stage, "broadcast"), Options{}, out, false, nil); err == nil || !strings.Contains(err.Error(), "thread pane") {
		t.Fatalf("broadcast reply was accepted outside the thread pane: %v", err)
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
	items, err := os.ReadDir(filepath.Join(out.Stage, "search"))
	if err != nil || len(items) == 0 {
		t.Fatalf("search context: %v %v", items, err)
	}
}

func TestReopenThreadPaneAfterParentNavigation(t *testing.T) {
	t.Chdir(t.TempDir())
	b, err := OpenBrowser(Options{Width: 800, Height: 600, Scale: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = b.setup(); err != nil {
		t.Fatal(err)
	}
	parent := "1730000000.000000"
	markup := `<style>body{margin:0}[data-qa=message_pane],[data-qa=threads_flexpane]{height:600px}[data-qa=slack_kit_scrollbar]{height:500px;overflow:auto}[data-qa=virtual-list-item]{height:100px}</style>` +
		`<div data-qa="message_pane"><div data-qa="slack_kit_scrollbar"><div data-qa="virtual-list-item"><article data-qa="message_container" data-msg-ts="` + parent + `">Parent</article><button data-qa="reply_bar_view_thread" onclick="document.body.insertAdjacentHTML('beforeend','<div data-qa=threads_flexpane><div data-qa=slack_kit_scrollbar><div data-qa=message_container data-msg-ts=` + parent + `>Parent</div><div data-qa=message_container data-msg-ts=1730000001.000000>Reply</div></div></div>')">Replies</button></div></div></div>`
	if err = b.navigate("data:text/html," + url.PathEscape(markup)); err != nil {
		t.Fatal(err)
	}
	thread, err := b.reopenThreadPane(parent)
	if err != nil {
		t.Fatal(err)
	}
	if messages, err := b.nodes(`[data-qa="message_container"]`, thread); err != nil || len(messages) != 2 {
		t.Fatalf("reopened thread: %d messages, %v", len(messages), err)
	}

	ariaMarkup := `<style>body{margin:0}[data-qa=message_pane],[data-qa=threads_flexpane]{height:600px}[data-qa=slack_kit_scrollbar]{height:500px;overflow:auto}[data-qa=virtual-list-item]{height:100px}</style>` +
		`<div data-qa="message_pane"><div data-qa="slack_kit_scrollbar"><div data-qa="virtual-list-item"><article data-qa="message_container" data-msg-ts="` + parent + `">Parent</article><button aria-label="Open thread" onclick="document.body.insertAdjacentHTML('beforeend','<div data-qa=threads_flexpane><div data-qa=slack_kit_scrollbar><div data-qa=message_container data-msg-ts=` + parent + `>Parent</div><div data-qa=message_container data-msg-ts=1730000001.000000>Reply</div></div></div>')">Replies</button></div></div></div>`
	if err = b.navigate("data:text/html," + url.PathEscape(ariaMarkup)); err != nil {
		t.Fatal(err)
	}
	thread, err = b.reopenThreadPane(parent)
	if err != nil {
		t.Fatal(err)
	}
	if messages, err := b.nodes(`[data-qa="message_container"]`, thread); err != nil || len(messages) != 2 {
		t.Fatalf("reopened aria thread: %d messages, %v", len(messages), err)
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

func TestClickIDDoesNotHitOverlay(t *testing.T) {
	t.Chdir(t.TempDir())
	b, err := OpenBrowser(Options{Width: 800, Height: 600, Scale: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	markup := `<style>button{position:fixed;inset:100px auto auto 100px;width:200px;height:100px}#overlay{z-index:1}</style>` +
		`<button id="target" onclick="window.targetClicks=(window.targetClicks||0)+1">Target</button>` +
		`<button id="overlay" onclick="window.overlayClicks=(window.overlayClicks||0)+1">Overlay</button>`
	if err = b.navigate("data:text/html," + url.PathEscape(markup)); err != nil {
		t.Fatal(err)
	}
	target, err := b.first("#target", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.clickID(target); err != nil {
		t.Fatal(err)
	}
	clicks, err := chromedp.Run(b.ctx, chromedp.Evaluate[struct{ Target, Overlay int }](`({Target:window.targetClicks||0,Overlay:window.overlayClicks||0})`))
	if err != nil || clicks.Target != 1 || clicks.Overlay != 0 {
		t.Fatalf("clicks = %+v: %v", clicks, err)
	}
}

func TestSlackAPIInventory(t *testing.T) {
	searchCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/auth.test":
			_, _ = io.WriteString(w, `{"ok":true,"url":"https://yourorg.slack.com/","user":"alice","user_id":"U1"}`)
		case "/search.messages":
			searchCalls++
			if r.URL.Query().Get("query") != `keyword -from:<@U1>` || r.URL.Query().Get("count") != "100" || r.URL.Query().Get("sort_dir") != "asc" {
				http.Error(w, "bad query", http.StatusBadRequest)
				return
			}
			if r.URL.Query().Get("page") == "1" {
				_, _ = io.WriteString(w, `{"ok":true,"messages":{"matches":[{"permalink":"https://yourorg.slack.com/archives/C123/p1730000000000000","text":"<@U1> direct","ts":"1730000000.000000","user":"U2"}],"total":2,"pagination":{"page_count":2,"total_count":2}}}`)
			} else {
				_, _ = io.WriteString(w, `{"ok":true,"messages":{"matches":[{"permalink":"https://yourorg.slack.com/archives/C123/p1740000000000000?thread_ts=1735000000.000000&cid=C123","text":"indirect keyword","ts":"1740000000.000000","user":"U2"}],"total":2,"pagination":{"page_count":2,"total_count":2}}}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	api := slackAPI{baseURL: server.URL + "/", token: "secret", client: server.Client()}
	hits, total, leadingExcluded, urlExcluded, err := inventory(context.Background(), Options{Domain: "yourorg.slack.com", Query: "keyword", ExcludeAuthor: "alice", ExcludeLeading: "alice", OldestFirst: true}, api)
	if err != nil || total != 2 || leadingExcluded != 1 || urlExcluded != 0 || len(hits) != 1 || hits[0].TS != "1740000000.000000" || hits[0].Parent != "1735000000.000000" || searchCalls != 2 {
		t.Fatalf("inventory = %+v, total=%d leading=%d URLs=%d calls=%d: %v", hits, total, leadingExcluded, urlExcluded, searchCalls, err)
	}
}

func TestSlackAPIFailsOnCollapsedResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"messages":{"matches":[{"permalink":"https://yourorg.slack.com/archives/C123/p1730000000000000","ts":"1730000000.000000"}],"total":2,"pagination":{"page_count":1,"total_count":2}}}`)
	}))
	defer server.Close()
	api := slackAPI{baseURL: server.URL + "/", token: "secret", client: server.Client()}
	_, _, err := api.search(context.Background(), "keyword", false)
	if err == nil || !strings.Contains(err.Error(), "reported 2 hits but returned 1") {
		t.Fatalf("collapsed search error = %v", err)
	}
}

func TestSearchResumeStateAndThreadGrouping(t *testing.T) {
	root := t.TempDir()
	o := Options{Domain: "yourorg.slack.com", Query: "keyword before:2026-10-16", Output: root, Width: 1, Height: 1, OutputWidth: 1, Scale: 1, Before: 5, After: 20}
	permalinks := []string{
		"https://yourorg.slack.com/archives/C123/p1740000000000000?thread_ts=1735000000.000000&cid=C123",
		"https://yourorg.slack.com/archives/C456/p1730000000000000",
		"https://yourorg.slack.com/archives/C789/p1750000000000000?thread_ts=1725000000.000000&cid=C789",
		"https://yourorg.slack.com/archives/C123/p1741000000000000?thread_ts=1735000000.000000&cid=C123",
		"https://yourorg.slack.com/archives/C456/p1760000000000000",
	}
	var hits []hit
	for _, permalink := range permalinks {
		h, err := parseHit(permalink, o.Domain)
		if err != nil {
			t.Fatal(err)
		}
		hits = append(hits, h)
	}
	grouped, samePane, reused := groupSearchHits(hits)
	if samePane != 2 || reused != 1 || grouped[0].Parent != grouped[1].Parent || grouped[0].Channel != grouped[1].Channel || grouped[2].Channel != grouped[3].Channel {
		t.Fatalf("thread grouping = %+v, same pane %d, reused %d", grouped, samePane, reused)
	}
	coverage := map[string]bool{"1739000000.000000": true, grouped[0].TS: true, "1742000000.000000": true}
	if !contextCovers(coverage, grouped[0], 1, 1) || contextCovers(coverage, grouped[0], 2, 1) {
		t.Fatalf("context coverage mismatch: %+v", coverage)
	}
	source, target := filepath.Join(root, "source"), filepath.Join(root, "target")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "000001.png"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := linkSearchScreens(source, target); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(filepath.Join(source, "000001.png"))
	if err != nil {
		t.Fatal(err)
	}
	linked, err := os.Stat(filepath.Join(target, "000001.png"))
	if err != nil || !os.SameFile(original, linked) {
		t.Fatalf("search screens were not hard-linked: %v", err)
	}
	out, err := NewOutput(root)
	if err != nil {
		t.Fatal(err)
	}
	out.Dates["2026-10-02"] = true
	out.Saved["FABC"] = true
	state := &searchState{
		Version: searchStateVersion, Spec: searchOptions(o), Stage: out.Stage, Total: 10, Targets: 5, ThreadReplies: 3,
		Hits: grouped, Completed: map[string]bool{grouped[0].Key: true}, Dates: out.Dates, Saved: out.Saved, Missing: out.Missing,
	}
	path, err := searchStatePath(o)
	if err != nil {
		t.Fatal(err)
	}
	if err = saveSearchState(path, state, out); err != nil {
		t.Fatal(err)
	}
	completed := filepath.Join(out.Stage, grouped[0].Key)
	if err = os.Mkdir(completed, 0700); err != nil {
		t.Fatal(err)
	}
	writeTestPNG(t, filepath.Join(completed, "000001.png"), 1, 1)
	if err = os.WriteFile(filepath.Join(out.Stage, "file_FABC_saved.txt"), []byte("saved"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(out.Stage, "file_FEXTRA_notes.txt"), []byte("saved"), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, resumed, found, err := loadSearchState(path, o)
	if err != nil || !found || loaded.Stage != out.Stage || len(loaded.Completed) != 1 || !loaded.Dates["2026-10-02"] || !loaded.Saved["FABC"] || !loaded.Saved["FEXTRA"] || resumed.Stage != out.Stage {
		t.Fatalf("resume state = %+v, output = %+v, found=%v: %v", loaded, resumed, found, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || bytes.Contains(data, []byte("https://")) {
		t.Fatalf("resume state retained a private permalink: %v", err)
	}
	if err = os.Remove(filepath.Join(completed, "000001.png")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = loadSearchState(path, o); err == nil || !strings.Contains(err.Error(), "no screenshots") {
		t.Fatalf("resume accepted a completed hit without PNGs: %v", err)
	}
	changed := o
	changed.After++
	changedPath, err := searchStatePath(changed)
	if err != nil || changedPath == path {
		t.Fatalf("changed search reused state path: %q %q %v", path, changedPath, err)
	}
	changed = o
	changed.OldestFirst = true
	changedPath, err = searchStatePath(changed)
	if err != nil || changedPath == path {
		t.Fatalf("oldest-first search reused state path: %q %q %v", path, changedPath, err)
	}
}

func TestSearchHitWorklistChannelMessagesOnly(t *testing.T) {
	hits := []hit{
		{TS: "1", Parent: "1", Key: "channel-complete"},
		{TS: "2", Parent: "1", Key: "thread-reply"},
		{TS: "3", Parent: "3", Key: "channel-pending"},
	}
	completed := map[string]bool{"channel-complete": true}

	pending, remaining := searchHitWorklist(hits, completed, true)
	if len(pending) != 1 || pending[0] != 2 || remaining != 2 {
		t.Fatalf("channel-only worklist = %v, remaining %d; want [2], 2", pending, remaining)
	}

	pending, remaining = searchHitWorklist(hits, completed, false)
	if len(pending) != 2 || pending[0] != 1 || pending[1] != 2 || remaining != 2 {
		t.Fatalf("full worklist = %v, remaining %d; want [1 2], 2", pending, remaining)
	}
}

func TestFailedSearchAttemptRollsBackOutput(t *testing.T) {
	out, err := NewOutput(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out.Dates["2026-10-01"] = true
	out.Saved["FOLD"] = true
	before := snapshotSearchAttempt(out)
	out.Dates["2026-10-02"] = true
	out.Saved["FNEW"] = true
	out.Missing["missing"] = MissingFile{Reason: "fixture"}
	if err = os.WriteFile(filepath.Join(out.Stage, "file_FNEW_notes.txt"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(out.Stage, "hit")
	if err = os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err = rollbackSearchAttempt(out, before, directory); err != nil {
		t.Fatal(err)
	}
	if len(out.Dates) != 1 || !out.Dates["2026-10-01"] || len(out.Saved) != 1 || !out.Saved["FOLD"] || len(out.Missing) != 0 {
		t.Fatalf("failed attempt state remained: dates=%v saved=%v missing=%v", out.Dates, out.Saved, out.Missing)
	}
	if _, err = os.Stat(filepath.Join(out.Stage, "file_FNEW_notes.txt")); !os.IsNotExist(err) {
		t.Fatalf("failed attempt attachment remained: %v", err)
	}
	if _, err = os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("failed attempt screenshots remained: %v", err)
	}
}

func TestSearchAPIFilters(t *testing.T) {
	search, _, err := NewCommand().Find([]string{"search"})
	if err != nil || search.Flags().Lookup("exclude-author") == nil || search.Flags().Lookup("exclude-leading-mention") == nil || search.Flags().Lookup("oldest-first") == nil || search.Flags().Lookup("exclude-bots") != nil {
		t.Fatalf("search API flags invalid: %v", err)
	}
	if got := keychainArgs("yourorg.slack.com"); strings.Join(got, " ") != "find-internet-password -l slack-auto-screenshot-user-token -s yourorg.slack.com -w" {
		t.Fatalf("keychain args = %q", got)
	}
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"<@U1> hello", true},
		{"  <@U1|alice> hello", true},
		{"hello <@U1>", false},
		{"<@U10> hello", false},
	} {
		if got := startsWithUserMention(tc.text, "U1"); got != tc.want {
			t.Errorf("startsWithUserMention(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"https://status.example.com/service2/1", true},
		{"<https://status.example.com/service2/1|dashboard>", true},
		{"service2 is down: https://status.example.com/service", false},
		{"<https://status.example.com/service|service2 dashboard>", false},
		{"service2 without a link", false},
		{"https://status.example.com/service/1", false},
		{"no keyword and no link", false},
	} {
		if got := keywordOnlyInURL(tc.text, "service2"); got != tc.want {
			t.Errorf("keywordOnlyInURL(%q) = %v, want %v", tc.text, got, tc.want)
		}
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
