package app

import "fmt"

// Slack's private DOM contract lives here so UI changes have one place to update.
const (
	slackUIReady                 = `[data-qa="top_nav_search"], [data-qa="message_pane"]`
	slackUIMessagePane           = `[data-qa="message_pane"]`
	slackUIThreadPane            = `[data-qa="threads_flexpane"]`
	slackUIMessage               = `[data-qa="message_container"]`
	slackUIConversationMessages  = slackUIMessagePane + " " + slackUIMessage
	slackUIThreadMessages        = slackUIThreadPane + " " + slackUIMessage
	slackUIMessageWithTimestamp  = slackUIMessage + `[data-msg-ts="%s"]`
	slackUIMessagesWithTimestamp = slackUIMessage + `[data-msg-ts]`
	slackUIScrollbar             = `[data-qa="slack_kit_scrollbar"]`
	slackUIThreadReplyBar        = `:is([data-qa="reply_bar_view_thread"],button[aria-label*="thread" i],button[aria-label*="repl" i],[role="button"][aria-label*="thread" i],[role="button"][aria-label*="repl" i])`
	slackUIVirtualListItem       = `[data-qa="virtual-list-item"]`
	slackUIThreadCloseButtons    = `[data-qa="close_flexpane"], [data-qa="unstyled-button"][aria-label="Close"]`
	slackUIAvailabilityDialog    = `[role="dialog"]`
	slackUIAvailabilityButtons   = `button, [role="button"]`
	slackUIAvailabilityClose     = `[data-qa="sk_close_modal_button"]`
	slackUIAutocomplete          = `.c-search_modal--autocomplete`
	slackUIChannelTitle          = `[data-qa="channel_name"] .p-view_header__channel_title, [data-qa="dm_title"]`
	slackUIChannelSearchInput    = `[data-qa="focusable_search_input"] [data-qa="texty_input"]`
	slackUITopNavSearch          = `[data-qa="top_nav_search"]`
	slackUIChannelOptions        = `[role="option"]:has([data-qa="small_channel_entity"])`
	slackUISmallChannelEntity    = `[data-qa="small_channel_entity"]`
	slackUIAttachmentBlock       = `[data-qa="message_kit_files"]`
	slackUIAttachmentLinks       = `a[href*="files.slack.com/files-pri/"],a[data-qa="message_file_link"]`
	slackUIFileName              = `[data-qa="file_name"]`
	slackUIFileCollapse          = `[data-qa="file-collapse-button"]`
	slackUIThreadReplyItems      = `[data-qa="virtual-list-item"]:has(` + slackUIThreadReplyBar + `)`
)

const captureSidebarCSS = `[data-qa="search_tip_banner"]{display:none!important}` +
	`.p-client_workspace__tabpanel:not(:has(` + slackUIThreadPane + `)){grid-template-columns:0 minmax(0,1fr)!important}` +
	`.p-client_workspace__tabpanel:not(:has(` + slackUIThreadPane + `)) .p-view_contents--sidebar{display:none!important}`

const slackPendingFileImages = `Array.from(document.querySelectorAll('[data-qa="message_kit_files"] img')).filter(i => {const r=i.getBoundingClientRect();return r.width>0&&r.height>0&&r.top<innerHeight&&r.bottom>0&&r.left<innerWidth&&r.right>0&&!i.complete}).length`

const slackReopenThreadClick = `function(){let root=this,bar=null;while(root&&root.getAttribute('data-qa')!=='message_pane'){bar=root.querySelector('[data-qa="reply_bar_view_thread"],button[aria-label*="thread" i],button[aria-label*="repl" i],[role="button"][aria-label*="thread" i],[role="button"][aria-label*="repl" i]');if(bar)break;root=root.parentElement}if(!bar)throw new Error("reply bar not found");bar.click()}`

const slackDialogClickVisibilityCheck = `function(){const r=this.getBoundingClientRect(),hit=document.elementFromPoint(r.x+r.width/2,r.y+r.height/2);if(hit!==this&&!this.contains(hit))throw new Error("dialog choice is obscured")}`

func slackMessageAt(ts string) string { return fmt.Sprintf(slackUIMessageWithTimestamp, ts) }

func slackScrollpaneFor(ts string) string {
	return fmt.Sprintf(`%s:has(%s)`, slackUIScrollbar, slackMessageAt(ts))
}

func slackThreadReplyButton(ts string) string {
	return fmt.Sprintf(`%s:has(%s) %s`, slackUIVirtualListItem, slackMessageAt(ts), slackUIThreadReplyBar)
}

func slackThreadIsolationCSS(background string) string {
	return slackUIThreadPane + ` {position:fixed!important;inset:0 auto auto 0!important;width:100vw!important;height:100vh!important;z-index:100000!important;background:rgb(` + background + `)!important}.p-tab_rail,.p-workspace_banner__desktop-notifications{display:none!important}`
}
