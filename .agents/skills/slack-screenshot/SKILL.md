---
name: slack-screenshot
description: Use when the user asks to save screenshots of a Slack channel, DM, group DM, thread history, or search results. Operate this repository's Go screenshot CLI, verify the saved output, and report missing attachments. Do not use for sending Slack messages, exporting message text through an API, or ordinary discussion about Slack.
compatibility: Requires Google Chrome, Go 1.27+, filesystem access, and a manually authenticated local Slack session.
---

# Capture Slack screenshots

Use the Go CLI, not Slack APIs or reconstructed images. It controls a dedicated Chrome profile; never inspect, copy, upload, or commit browser credentials or private screenshots. Resolve the repository root as `../../..` from this skill directory. Run commands with the terminal tool's working directory set to that root so `.slack-browser/` and the default `captures/` resolve correctly. Do not hard-code the developer's home directory.

## Procedure

1. Identify the target and scope from the request: a bare workspace domain plus `#channel`, or a canonical `https://app.slack.com/client/<workspace-id>/<conversation-id>` URL for a channel/DM/group DM; for search, a bare workspace domain plus a query. Ask for a missing or ambiguous target or date range. Do not invent IDs, silently choose a workspace, or treat a legacy `/archives/...` URL as a canonical conversation URL.
2. Check available disk space before a large capture. Run `make build`, then use `./bin/slack-auto-screenshot ...`. Quote every user-provided argument as shell data; never interpolate it into shell syntax. Keep the default 1440×2560 CSS-pixel viewport, device scale 1.5, and 1440-pixel PNG output unless requested otherwise. This WQHD portrait setup avoided the large blank white regions seen at the old 2160×3840 CSS-pixel viewport with scale 2; never choose that old combination without explicit approval and visual verification.
3. Use the applicable command below. Omit `--from` and `--to` for **all history**; otherwise provide **both** inclusive dates. Search uses Slack's `after:`/`before:` query operators, not capture's date flags. `-B`/`-A` set the before/after counts (default 20 each), `-C` sets both defaults, and `--limit` bounds hits only if requested. Add `--gzip` only when an archive is requested; originals remain on disk. `--theme light|dark`, `--aspect-ratio`, `--width`, `--scale`, `--output-width`, and `--output` are optional; `--output-width 0` selects native-resolution output when explicitly requested. Use the default ignored `captures/` unless the user specifies another safe location.
4. If the CLI reports authentication is required, **stop** and request manual sign-in using `./bin/slack-auto-screenshot login <workspace-domain>` in a visible Chrome window. If only a conversation URL was supplied, ask for its workspace domain; do not guess it from the workspace ID. Do not enter passwords, copy cookies, reuse a personal Chrome profile, or start an unattended login that waits indefinitely.
5. After the command exits, use its reported final directory, not the newest folder by guesswork. Read `COMPLETE.txt` and compare its date range and counts with the request. If it warns of possible screenshot gaps, report the count as best-effort coverage, not complete coverage. Confirm numbered channel/search PNGs and `thread_*.png` when threads were reported; count distinct parent IDs, not PNG files, for the thread count. Inspect `MISSING_FILES.txt` if present and report its count/path; missing attachments do not cancel successful screenshots. For `--gzip`, confirm the adjacent `.tar.gz` exists and is readable. If the command fails or leaves `.incomplete-*`/`INCOMPLETE.txt` without `COMPLETE.txt`, report **partial or failed**, preserve the artifacts, and do not claim completion. Never delete previous captures on your own.
6. Report the absolute output path, photographed date range, screenshot/thread/attachment counts, missing-file count, and whether an archive was produced. Do not paste message text, screenshot contents, credential paths, or private attachment links into the response. A completion marker does **not** prove that every Slack message ID was reconciled against the images.

## Commands

Replace placeholders with the user's values. Execute from the repository root:

```sh
make build
./bin/slack-auto-screenshot capture '<workspace-domain>' '#channel-name'
./bin/slack-auto-screenshot capture 'https://app.slack.com/client/<workspace-id>/<conversation-id>' --from 2026-10-01 --to 2026-10-31
./bin/slack-auto-screenshot search '<workspace-domain>' '<search query>' -B 10 -A 20 --limit 5
```

The folder name reflects **dates actually visible in saved frames**, which may extend outside a requested filter because of boundary messages or entire captured threads. Thread PNGs and downloadable original attachments are included by `capture`; search gathers files from photographed hit context. File-detail-only attachments can be listed as missing instead of downloaded. Prefer the CLI's `--help` output when checking current flags. The historical `in:` search-operator problem and Slack UI changes can still prevent some searches; do not mask failures.
