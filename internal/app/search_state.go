package app

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"strings"
)

type searchSpec struct {
	Domain, ExcludeAuthor, ExcludeLeading, ExcludeURLSubstring string
	Theme                                                      string
	Width, Height, OutputWidth, Before, After, Limit, Offset   int
	Scale                                                      float64
	OldestFirst                                                bool
}

type searchState struct {
	Version                       int
	Spec                          searchSpec
	Stage                         string
	Total, Targets, ThreadReplies int
	LeadingExcluded, URLExcluded  int
	Hits                          []hit
	Completed, Dates, Saved       map[string]bool
	Missing                       map[string]MissingFile
}

func searchOptions(o Options) searchSpec {
	return searchSpec{
		Domain: o.Domain, ExcludeAuthor: o.ExcludeAuthor, ExcludeLeading: o.ExcludeLeading,
		ExcludeURLSubstring: o.ExcludeURLSubstring, Theme: o.Theme, Width: o.Width, Height: o.Height,
		OutputWidth: o.OutputWidth, Before: o.Before, After: o.After, Limit: o.Limit, Offset: o.Offset, Scale: o.Scale,
		OldestFirst: o.OldestFirst,
	}
}

func searchStatePath(o Options) (string, error) {
	// Keep this fingerprint encoding stable so existing interrupted searches can resume.
	data, err := json.Marshal(struct {
		Domain, Query, ExcludeAuthor, ExcludeLeading, ExcludeURLSubstring string
		Theme                                                             string
		Width, Height, OutputWidth, Before, After, Limit, Offset          int
		Scale                                                             float64
		OldestFirst                                                       bool
	}{
		Domain: o.Domain, Query: o.Query, ExcludeAuthor: o.ExcludeAuthor, ExcludeLeading: o.ExcludeLeading,
		ExcludeURLSubstring: o.ExcludeURLSubstring, Theme: o.Theme, Width: o.Width, Height: o.Height,
		OutputWidth: o.OutputWidth, Before: o.Before, After: o.After, Limit: o.Limit, Offset: o.Offset,
		Scale: o.Scale, OldestFirst: o.OldestFirst,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return filepath.Join(o.Output, fmt.Sprintf(".search-%x.json", sum[:8])), nil
}

func saveSearchState(path string, state *searchState, out *Output) error {
	state.Dates, state.Saved, state.Missing = out.Dates, out.Saved, out.Missing
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".search-state-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func validStoredHit(h hit) bool {
	return slackChannel.MatchString(h.Channel) && messageTimestamp.MatchString(h.TS) &&
		messageTimestamp.MatchString(h.Parent) && h.Key == h.Channel+"-"+strings.ReplaceAll(h.TS, ".", "") && h.Permalink == ""
}

func validateSearchScreens(stage string, h hit, o Options) error {
	directory := filepath.Join(stage, h.Key)
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("completed search hit %s has no screenshot directory", h.Key)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	scale := o.Scale
	if o.OutputWidth != 0 {
		scale = float64(o.OutputWidth) / float64(o.Width)
	}
	wantWidth, wantHeight := int(math.Round(float64(o.Width)*scale)), int(math.Round(float64(o.Height)*scale))
	frames := 0
	for _, entry := range entries {
		frames++
		wantName := fmt.Sprintf("%06d.png", frames)
		if entry.Name() != wantName || !entry.Type().IsRegular() {
			return fmt.Errorf("completed search hit %s contains an invalid screenshot", h.Key)
		}
		file, err := os.Open(filepath.Join(directory, entry.Name()))
		if err != nil {
			return err
		}
		config, format, decodeErr := image.DecodeConfig(file)
		closeErr := file.Close()
		if decodeErr != nil || format != "png" || config.Width != wantWidth || config.Height != wantHeight {
			return fmt.Errorf("completed search hit %s contains an invalid PNG", h.Key)
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if frames == 0 {
		return fmt.Errorf("completed search hit %s has no screenshots", h.Key)
	}
	return nil
}

func loadSearchState(path string, o Options) (*searchState, *Output, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	var state searchState
	if err = json.Unmarshal(data, &state); err != nil {
		return nil, nil, false, fmt.Errorf("read search resume state: %w", err)
	}
	if state.Version != searchStateVersion || state.Spec != searchOptions(o) {
		return nil, nil, false, errors.New("search resume state does not match this command")
	}
	stage := filepath.Clean(state.Stage)
	if filepath.Dir(stage) != filepath.Clean(o.Output) || !strings.HasPrefix(filepath.Base(stage), ".incomplete-") {
		return nil, nil, false, errors.New("search resume state contains an invalid capture directory")
	}
	info, err := os.Lstat(stage)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, false, errors.New("search resume capture directory is unavailable")
	}
	keys := map[string]hit{}
	for _, h := range state.Hits {
		if _, duplicate := keys[h.Key]; !validStoredHit(h) || duplicate {
			return nil, nil, false, errors.New("search resume state contains an invalid hit")
		}
		keys[h.Key] = h
	}
	for key := range state.Completed {
		h, ok := keys[key]
		if !ok {
			return nil, nil, false, errors.New("search resume state contains an unknown completed hit")
		}
		if err = validateSearchScreens(stage, h, o); err != nil {
			return nil, nil, false, fmt.Errorf("search resume state is incomplete: %w", err)
		}
	}
	if state.Completed == nil {
		state.Completed = map[string]bool{}
	}
	if state.Dates == nil {
		state.Dates = map[string]bool{}
	}
	if state.Saved == nil {
		state.Saved = map[string]bool{}
	}
	if state.Missing == nil {
		state.Missing = map[string]MissingFile{}
	}
	entries, err := os.ReadDir(stage)
	if err != nil {
		return nil, nil, false, err
	}
	foundSaved := map[string]bool{}
	for _, entry := range entries {
		name := strings.TrimPrefix(entry.Name(), "file_")
		if entry.Type().IsRegular() && name != entry.Name() {
			if end := strings.IndexByte(name, '_'); end > 1 && name[0] == 'F' {
				foundSaved[name[:end]] = true
				state.Saved[name[:end]] = true
			}
		}
	}
	for key := range state.Saved {
		if !savedFileID.MatchString(key) || !foundSaved[key] {
			return nil, nil, false, fmt.Errorf("search resume state references a missing attachment: %s", key)
		}
	}
	out := &Output{Root: o.Output, Stage: stage, Dates: state.Dates, Saved: state.Saved, Missing: state.Missing}
	return &state, out, true, nil
}
