# Slack Auto Screenshot

A Go CLI that saves PNG screenshots of Slack as rendered in Chrome. It does not retrieve message bodies through the Slack API.

- Capture available channel, DM, and group DM history, including threads and downloadable attachments. Omit dates to capture all available history.
- Capture search results with surrounding messages.

Run `make build`, then use `login` to sign in manually through a dedicated Chrome profile.

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
      --aspect-ratio string   Viewport aspect ratio at a fixed 2160 CSS-pixel width (default "9:16")
      --from string           Inclusive start date (YYYY-MM-DD; requires --to)
      --gzip                  Archive the folder as tar.gz while retaining PNGs
  -h, --help                  help for capture
      --output string         Parent output directory (default "captures")
  -s, --scale int             Device scale factor (1–4; default 2) (default 2)
      --theme string          Capture-page theme: light or dark
      --to string             Inclusive end date (YYYY-MM-DD; requires --from)
```

```console
Usage:
  slack-auto-screenshot search <workspace-domain> <query> [flags]

Flags:
  -A, --after int             Number of messages after a hit (default 20)
      --aspect-ratio string   Viewport aspect ratio at a fixed 2160 CSS-pixel width (default "9:16")
  -B, --before int            Number of messages before a hit (default 20)
  -C, --context int           Default number of messages before and after a hit (default 20)
      --gzip                  Archive the folder as tar.gz while retaining PNGs
  -h, --help                  help for search
      --limit int             Maximum number of search hits (default: all)
      --output string         Parent output directory (default "captures")
  -s, --scale int             Device scale factor (1–4; default 2) (default 2)
      --theme string          Capture-page theme: light or dark
```

## License

[MIT](LICENSE).
