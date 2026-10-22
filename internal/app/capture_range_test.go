package app

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

func TestCaptureRangeSeeksToStartDate(t *testing.T) {
	t.Chdir(t.TempDir())
	b, err := OpenBrowser(Options{Width: 800, Height: 600, Scale: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = b.setup(); err != nil {
		t.Fatal(err)
	}
	var html strings.Builder
	html.WriteString(`<style>body{margin:0}#pane{height:500px;overflow-y:auto}article{height:110px}</style><div id="pane" data-qa="slack_kit_scrollbar">`)
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.Local).Unix()
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
	if _, err = chromedp.Run(b.ctx, chromedp.Evaluate[chromedp.Void](`{const p=document.getElementById('pane');p.scrollTop=p.scrollHeight}`)); err != nil {
		t.Fatal(err)
	}
	before, err := chromedp.Run(b.ctx, chromedp.Evaluate[float64](`document.getElementById('pane').scrollTop`))
	if err != nil {
		t.Fatal(err)
	}
	if err = b.captureStart(pane, "2026-10-20"); err != nil {
		t.Fatal(err)
	}
	after, err := chromedp.Run(b.ctx, chromedp.Evaluate[float64](`document.getElementById('pane').scrollTop`))
	visible, visibleErr := b.visible(pane, nil)
	if err != nil || visibleErr != nil || after <= 0 || after >= before || len(visible) == 0 || visible[0].day > "2026-10-20" || visible[len(visible)-1].day < "2026-10-20" {
		t.Fatalf("range start was not positioned: before=%f after=%f visible=%+v err=%v %v", before, after, visible, err, visibleErr)
	}
}
