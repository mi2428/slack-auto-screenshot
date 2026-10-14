package app

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

type MissingFile struct{ Timestamp, ID, Name, Reason, Source string }

type Output struct {
	Root, Stage string
	Dates       map[string]bool
	Saved       map[string]bool
	Missing     map[string]MissingFile
	Gaps        []string
}

func NewOutput(root string) (*Output, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(root, ".incomplete-")
	if err != nil {
		return nil, err
	}
	return &Output{Root: root, Stage: stage, Dates: map[string]bool{}, Saved: map[string]bool{}, Missing: map[string]MissingFile{}}, nil
}

func safeName(name string, max int) string {
	var chars []rune
	for _, r := range name {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			r = '_'
		}
		chars = append(chars, r)
		if len(chars) >= max {
			break
		}
	}
	return strings.Trim(string(chars), " .")
}

func (o *Output) RecordMissing(key string, m MissingFile) {
	if o.Saved[key] || o.Missing[key].Reason != "" {
		return
	}
	o.Missing[key] = m
	fmt.Fprintf(os.Stderr, "Attachment missing: message %s / %s - %s\n", m.Timestamp, m.ID, m.Reason)
}

func (o *Output) WriteMissing() error {
	if len(o.Missing) == 0 {
		return nil
	}
	keys := make([]string, 0, len(o.Missing))
	for key := range o.Missing {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "Missing attachments: %d\nMessage ID\tFile ID\tFile name\tReason\tSource URL (without query)\n", len(keys))
	for _, key := range keys {
		m := o.Missing[key]
		for i, field := range []string{m.Timestamp, m.ID, m.Name, m.Reason, m.Source} {
			if i > 0 {
				b.WriteByte('\t')
			}
			b.WriteString(strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(field))
		}
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(o.Stage, "MISSING_FILES.txt"), []byte(b.String()), 0600); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%d missing attachments recorded in MISSING_FILES.txt\n", len(keys))
	return nil
}

func (o *Output) Finish(name, summary string, compress bool) (string, error) {
	if len(o.Dates) == 0 {
		return "", os.RemoveAll(o.Stage)
	}
	if err := o.WriteMissing(); err != nil {
		return "", err
	}
	label := safeName(name, 40)
	if label == "" {
		return "", fmt.Errorf("output folder name is empty: %q", name)
	}
	days := make([]string, 0, len(o.Dates))
	for day := range o.Dates {
		days = append(days, day)
	}
	sort.Strings(days)
	period := days[0] + "_" + days[len(days)-1]
	output, err := os.MkdirTemp(o.Root, label+"_"+period+"_")
	if err != nil {
		return "", err
	}
	if err = os.Remove(output); err != nil {
		return "", err
	}
	if err = os.Rename(o.Stage, output); err != nil {
		return "", err
	}
	marker := filepath.Join(output, "COMPLETE.txt")
	if len(o.Gaps) > 0 {
		summary += fmt.Sprintf("\nWARNING: %d possible screenshot gap(s); capture continued from the next visible messages:\n%s", len(o.Gaps), strings.Join(o.Gaps, "\n"))
	}
	if err = os.WriteFile(marker, []byte(summary+"\nCaptured date range: "+period+"\n"), 0600); err == nil && compress {
		err = archive(output)
	}
	if err != nil {
		_ = os.Remove(marker)
		return "", fmt.Errorf("could not finalize output; PNGs remain at %s: %w", output, err)
	}
	return output, nil
}

func archive(output string) (err error) {
	f, err := os.CreateTemp(filepath.Dir(output), ".archive-*.partial")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(output, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, e := entry.Info()
		if e != nil {
			return e
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("special file in archive input: %s", path)
		}
		rel, e := filepath.Rel(filepath.Dir(output), path)
		if e != nil {
			return e
		}
		hdr, e := tar.FileInfoHeader(info, "")
		if e != nil {
			return e
		}
		hdr.Name = filepath.ToSlash(rel)
		if entry.IsDir() {
			hdr.Name += "/"
		}
		if e = tw.WriteHeader(hdr); e != nil {
			return e
		}
		if entry.IsDir() {
			return nil
		}
		file, e := os.Open(path)
		if e != nil {
			return e
		}
		defer file.Close()
		_, e = io.Copy(tw, file)
		return e
	})
	if e := tw.Close(); err == nil {
		err = e
	}
	if e := gz.Close(); err == nil {
		err = e
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), output+".tar.gz")
}
