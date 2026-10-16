package app

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type Options struct {
	Command, Domain, Target, Query string
	Output, Theme, From, To        string
	ExcludeAuthor, ExcludeLeading  string
	ExcludeURLSubstring            string
	Width, Height, OutputWidth     int
	Scale                          float64
	Before, After, Limit, Offset   int
	Gzip, OldestFirst              bool
}

var domainPattern = regexp.MustCompile(`(?i)^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.slack\.com$`)
var conversationPattern = regexp.MustCompile(`^https://app\.slack\.com/client/[^/?#]+/[CDG][A-Z0-9]+/?$`)
var ratioPart = regexp.MustCompile(`^\d+(?:\.\d+)?$`)
var authorHandle = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func validDomain(s string) bool {
	if !domainPattern.MatchString(s) {
		return false
	}
	switch strings.ToLower(strings.Split(s, ".")[0]) {
	case "app", "files", "www":
		return false
	}
	return true
}

func validDate(s string) bool {
	t, err := time.Parse("2006-01-02", s)
	return len(s) == 10 && err == nil && t.Format("2006-01-02") == s
}

func NewCommand() *cobra.Command {
	root := &cobra.Command{Use: "slack-auto-screenshot", Short: "Capture rendered Slack conversations", SilenceUsage: true, SilenceErrors: true}
	root.SetHelpCommand(&cobra.Command{Hidden: true})
	root.CompletionOptions.DisableDefaultCmd = true
	login := &cobra.Command{Use: "login <workspace-domain>", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		if !validDomain(args[0]) {
			return errors.New("login requires a workspace domain such as yourorg.slack.com")
		}
		return Run(Options{Command: "login", Domain: strings.ToLower(args[0])})
	}}
	root.AddCommand(login)

	capture := &cobra.Command{Use: "capture <conversation-URL> | <workspace-domain> '#channel'", Args: cobra.RangeArgs(1, 2)}
	co := Options{Command: "capture"}
	var ratio string
	common(capture, &co, &ratio)
	capture.Flags().StringVar(&co.From, "from", "", "Inclusive start date (YYYY-MM-DD; requires --to)")
	capture.Flags().StringVar(&co.To, "to", "", "Inclusive end date (YYYY-MM-DD; requires --from)")
	capture.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 && conversationPattern.MatchString(args[0]) {
			co.Target = args[0]
		} else if len(args) == 2 && validDomain(args[0]) && strings.HasPrefix(args[1], "#") && len(args[1]) > 1 {
			co.Domain, co.Target = strings.ToLower(args[0]), args[1]
		} else {
			return errors.New("provide a conversation URL or a workspace domain and #channel")
		}
		from, to := cmd.Flags().Changed("from"), cmd.Flags().Changed("to")
		if from != to || from && (!validDate(co.From) || !validDate(co.To) || co.From > co.To) {
			return errors.New("provide valid --from and --to dates together")
		}
		if err := validate(&co, ratio); err != nil {
			return err
		}
		return Run(co)
	}
	root.AddCommand(capture)

	search := &cobra.Command{Use: "search <workspace-domain> <query>", Args: cobra.ExactArgs(2)}
	so := Options{Command: "search", Before: 20, After: 20}
	var sr string
	var contextCount int
	common(search, &so, &sr)
	search.Flags().IntVarP(&contextCount, "context", "C", 20, "Default number of messages before and after a hit")
	search.Flags().IntVarP(&so.Before, "before", "B", 20, "Number of messages before a hit")
	search.Flags().IntVarP(&so.After, "after", "A", 20, "Number of messages after a hit")
	search.Flags().IntVar(&so.Limit, "limit", 0, "Maximum number of search hits (default: all)")
	search.Flags().IntVar(&so.Offset, "offset", 0, "Skip the first N filtered search hits")
	search.Flags().BoolVar(&so.OldestFirst, "oldest-first", false, "Capture the oldest filtered hits first")
	search.Flags().StringVar(&so.ExcludeAuthor, "exclude-author", "", "Exclude posts by a Slack username (@name)")
	search.Flags().StringVar(&so.ExcludeLeading, "exclude-leading-mention", "", "Exclude posts whose text starts with @name")
	search.Flags().StringVar(&so.ExcludeURLSubstring, "exclude-url-substring", "", "Exclude hits where this text occurs only inside URLs")
	search.RunE = func(cmd *cobra.Command, args []string) error {
		if !validDomain(args[0]) || strings.TrimSpace(args[1]) == "" {
			return errors.New("provide a workspace domain and a search query")
		}
		so.Domain, so.Query = strings.ToLower(args[0]), args[1]
		if cmd.Flags().Changed("exclude-author") {
			so.ExcludeAuthor = strings.TrimPrefix(so.ExcludeAuthor, "@")
			if !authorHandle.MatchString(so.ExcludeAuthor) {
				return errors.New("--exclude-author requires a Slack username such as @alice")
			}
		}
		if cmd.Flags().Changed("exclude-leading-mention") {
			so.ExcludeLeading = strings.TrimPrefix(so.ExcludeLeading, "@")
			if !authorHandle.MatchString(so.ExcludeLeading) {
				return errors.New("--exclude-leading-mention requires a Slack username such as @alice")
			}
		}
		if cmd.Flags().Changed("exclude-url-substring") {
			so.ExcludeURLSubstring = strings.TrimSpace(so.ExcludeURLSubstring)
			if so.ExcludeURLSubstring == "" {
				return errors.New("--exclude-url-substring must not be empty")
			}
		}
		if !cmd.Flags().Changed("before") {
			so.Before = contextCount
		}
		if !cmd.Flags().Changed("after") {
			so.After = contextCount
		}
		if contextCount < 0 || contextCount > 200 || so.Before < 0 || so.Before > 200 || so.After < 0 || so.After > 200 || so.Limit < 0 || so.Offset < 0 {
			return errors.New("context counts must be 0–200; the hit limit and offset must be nonnegative")
		}
		if err := validate(&so, sr); err != nil {
			return err
		}
		return Run(so)
	}
	root.AddCommand(search)
	return root
}

func common(cmd *cobra.Command, o *Options, ratio *string) {
	o.Width, o.Height = 1440, 2560
	cmd.Flags().IntVar(&o.Width, "width", 1440, "Viewport width in CSS pixels")
	cmd.Flags().IntVar(&o.OutputWidth, "output-width", 1440, "Output PNG width in pixels (0: native resolution)")
	cmd.Flags().Float64VarP(&o.Scale, "scale", "s", 1.5, "Device scale factor (1–4)")
	cmd.Flags().StringVar(ratio, "aspect-ratio", "9:16", "Viewport aspect ratio at the selected CSS-pixel width")
	cmd.Flags().StringVar(&o.Theme, "theme", "", "Capture-page theme: light or dark")
	cmd.Flags().StringVar(&o.Output, "output", "captures", "Parent output directory")
	cmd.Flags().BoolVar(&o.Gzip, "gzip", false, "Archive the folder as tar.gz while retaining PNGs")
}

func validate(o *Options, ratio string) error {
	if math.IsNaN(o.Scale) || math.IsInf(o.Scale, 0) || o.Scale < 1 || o.Scale > 4 {
		return errors.New("scale must be between 1 and 4")
	}
	if o.Width < 300 || o.Width > 8192 {
		return errors.New("viewport width must be between 300 and 8192 CSS pixels")
	}
	if o.OutputWidth != 0 && (o.OutputWidth < 300 || o.OutputWidth > 8192) {
		return errors.New("output width must be between 300 and 8192 pixels")
	}
	if o.Theme != "" && o.Theme != "light" && o.Theme != "dark" {
		return errors.New("theme must be light or dark")
	}
	parts := strings.Split(ratio, ":")
	if len(parts) != 2 || !ratioPart.MatchString(parts[0]) || !ratioPart.MatchString(parts[1]) {
		return errors.New("aspect ratio must use the width:height form, such as 16:9")
	}
	w, _ := strconv.ParseFloat(parts[0], 64)
	h, _ := strconv.ParseFloat(parts[1], 64)
	pixels := math.Round(float64(o.Width) * h / w)
	if w <= 0 || h <= 0 || math.IsNaN(pixels) || pixels < 300 || pixels > 8192 {
		return fmt.Errorf("aspect ratio %s must produce a height between 300 and 8192 CSS pixels", ratio)
	}
	o.Height = int(pixels)
	if strings.TrimSpace(o.Output) == "" {
		return errors.New("output directory must not be empty")
	}
	var err error
	o.Output, err = filepath.Abs(o.Output)
	return err
}
