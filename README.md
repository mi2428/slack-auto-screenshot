# Slack Auto Screenshot

A Go CLI that saves PNG screenshots of Slack as rendered in Chrome. Keyword search uses Slack's official API only to enumerate permalinks; message text is never saved.

- Capture available channel, DM, and group DM history, including threads and downloadable attachments. Omit dates to capture all available history.
- Capture search results with surrounding messages.
- Thread search hits retain Slack's split view, with the parent in the main pane and the matching reply in the thread pane.

Run `make build`, then use `login` to sign in manually through a dedicated Chrome profile.

Search also requires a Slack user token with `search:read`, stored as a macOS Internet Password named `slack-auto-screenshot-user-token`.
Its Website must be the workspace domain, such as `yourorg.slack.com`; search accepts no token flag, environment variable, or config file.

## Usage

Output goes to `captures/` by default.
Check `COMPLETE.txt` for capture counts and known coverage gaps, and `MISSING_FILES.txt` for unavailable attachments.

```console
Usage:
  slack-auto-screenshot login <workspace-domain> [flags]

Flags:
  -h, --help   help for login
```

```console
Usage:
  slack-auto-screenshot capture <conversation-URL> | <workspace-domain> '#channel' [flags]

Flags:
      --aspect-ratio string   Viewport aspect ratio at the selected CSS-pixel width (default "9:16")
      --from string           Inclusive start date (YYYY-MM-DD; requires --to)
      --gzip                  Archive the folder as tar.gz while retaining PNGs
  -h, --help                  help for capture
      --output string         Parent output directory (default "captures")
      --output-width int      Output PNG width in pixels (0: native resolution) (default 1440)
  -s, --scale float           Device scale factor (1–4) (default 1.5)
      --theme string          Capture-page theme: light or dark
      --to string             Inclusive end date (YYYY-MM-DD; requires --from)
      --width int             Viewport width in CSS pixels (default 1440)
```

```console
Usage:
  slack-auto-screenshot search <workspace-domain> <query> [flags]

Flags:
  -A, --after int             Number of messages after a hit (default 20)
      --aspect-ratio string   Viewport aspect ratio at the selected CSS-pixel width (default "9:16")
  -B, --before int            Number of messages before a hit (default 20)
  -C, --context int           Default number of messages before and after a hit (default 20)
      --exclude-author string Exclude posts by a Slack username (@name)
      --exclude-leading-mention string Exclude posts whose text starts with @name
      --exclude-url-substring string Exclude hits where this text occurs only inside URLs
      --gzip                  Archive the folder as tar.gz while retaining PNGs
  -h, --help                  help for search
      --limit int             Maximum number of search hits (default: all)
      --oldest-first          Capture the oldest filtered hits first
      --offset int            Skip the first N filtered search hits
      --output string         Parent output directory (default "captures")
      --output-width int      Output PNG width in pixels (0: native resolution) (default 1440)
  -s, --scale float           Device scale factor (1–4) (default 1.5)
      --theme string          Capture-page theme: light or dark
      --width int             Viewport width in CSS pixels (default 1440)
```

For example, `search yourorg.slack.com 'incident' --exclude-author @alice --exclude-leading-mention @alice` uses the authenticated user's ID for both exclusions.
Slack applies that user's search filters, including its bot-message preference, to API results.
The CLI refuses to capture when the API total and returned unique messages do not agree.

An interrupted search resumes when the same command and output directory are used again.
The saved inventory is reused, completed hits are skipped, and only remaining hits are retried.
Run one search command for the full result set instead of wrapping it in external `--offset`/`--limit` chunks; hits from the same thread are grouped so its open Slack view can be reused.

## License

[MIT](LICENSE).
