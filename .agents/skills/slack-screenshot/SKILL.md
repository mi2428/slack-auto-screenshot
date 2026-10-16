---
name: slack-screenshot
description: Use when the user asks to save screenshots of a Slack channel, DM, group DM, thread history, or keyword search results. Operate this repository's Go CLI, verify saved output, and report gaps or missing attachments. Do not send Slack messages, export message text, or reconstruct images.
compatibility: Requires macOS Keychain Access, Google Chrome, Go 1.27+, filesystem access, and a manually authenticated local Slack session.
---

# Capture Slack screenshots

Use this repository's Go CLI. Images must come from rendered Slack pages in its dedicated Chrome profile. For keyword search only, the CLI must enumerate target permalinks through Slack's official `search.messages` Web API, then open each permalink in Chrome; never search by driving Slack's browser UI. Never use Slack's undocumented internal APIs, inspect or copy browser credentials, reconstruct images, or commit credentials or private captures.

**Capturing must be completely passive. Never post, edit, or delete Slack messages, and never add, change, or remove emoji or any other reaction.**

Resolve the repository root as `../../..` from this skill directory and run commands there so `.slack-browser/` and `captures/` resolve correctly. Do not hard-code a home directory.

## Procedure

1. Identify the target and scope: a bare workspace domain plus `#channel`, a canonical `https://app.slack.com/client/<workspace-id>/<conversation-id>` URL, or a bare workspace domain plus a search query. Ask for a missing or ambiguous target or date range. Do not invent IDs or treat legacy `/archives/...` URLs as canonical conversation URLs.
2. Before a large capture, check disk space and run `make build`. Quote user-provided arguments as shell data. Keep the default 1440×2560 CSS-pixel viewport, scale 1.5, and 1440-pixel PNG output unless requested otherwise. Never use the obsolete 2160×3840 CSS viewport with scale 2 without explicit approval and visual verification.
3. Search requires a Slack **user token** with `search:read`. The CLI must retrieve it only as a macOS Internet Password whose label is exactly `slack-auto-screenshot-user-token` and whose server/Website is exactly the requested bare workspace domain. It must verify via `auth.test` that the token belongs to that workspace. Never accept a token through flags, environment variables, config files, stdin, logs, or browser storage.
4. Use only official Slack Web API endpoints. Keep returned message text in memory only as needed for filtering; persist only non-secret capture state such as channel ID, timestamp, permalink, and completion status with mode `0600`. Honor `429 Retry-After`. Reconcile the API-reported total with unique target IDs and fail closed rather than claim complete coverage when results are collapsed, capped, duplicated, or inconsistent.
5. Omit `--from` and `--to` for all channel history; otherwise provide both inclusive dates. Search dates belong in Slack query operators. `-B`/`-A` set search context, `-C` sets both defaults, and `--limit` bounds captured hits only when requested. For a search hit inside a thread, preserve Slack's split view with the parent visible in the main message pane and the matching reply visible in the thread pane; never isolate the thread into a context-free full-screen image. Add `--gzip` only when requested. Use the ignored `captures/` directory unless another safe output was specified.
6. If browser authentication is required, stop and request manual sign-in with `./bin/slack-auto-screenshot login <workspace-domain>` in visible Chrome. Never enter passwords, copy cookies, reuse a personal Chrome profile, or run an unattended login.
7. After exit, use the CLI's reported final directory. Read `COMPLETE.txt`; verify its query/date range and counts, numbered PNGs, thread PNGs where reported, and `MISSING_FILES.txt`. A warning means best-effort rather than complete coverage. Preserve failed `.incomplete-*` output and never delete previous captures without explicit permission.
8. Report the absolute output path, photographed date range, screenshot/thread/attachment counts, missing-file and screenshot-gap counts, and archive status. Do not paste message text, screenshot contents, tokens, credential paths, or private attachment links.

## Commands

```sh
make build
./bin/slack-auto-screenshot capture '<workspace-domain>' '#channel-name'
./bin/slack-auto-screenshot capture 'https://app.slack.com/client/<workspace-id>/<conversation-id>' --from 2026-10-01 --to 2026-10-31
./bin/slack-auto-screenshot search '<workspace-domain>' '<search query>' -B 10 -A 20 --limit 5
```

Search API responses are target inventory, not screenshots. Context and attachments still come from the rendered Slack permalink. Output dates may extend beyond query or channel boundaries because captured context and whole threads are retained.
