package rag

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
	"time"
)

// MaxDocBytes caps the size of a single source file.
const MaxDocBytes = 10 << 20

// Doc is one source document.
type Doc struct {
	ID      string
	Title   string
	Source  string
	Updated time.Time // zero when unknown
	Groups  []string  // empty means readable by everyone
	// Supersedes lists ids of older documents this one replaces. Replaced
	// documents are hidden from queries unless a filter asks for them.
	Supersedes []string
	Body       string
}

// Chunk is an indexed slice of a document.
type Chunk struct {
	ID      string
	DocID   string
	Title   string
	Section string
	Header  string // context line indexed with the text
	Source  string
	Updated time.Time
	Groups  []string
	Text    string   // screened text
	Removed []string // sentences removed by Screen
	// Superseded is set by Build when a newer document replaces this one.
	Superseded bool
}

var (
	headingRe  = regexp.MustCompile(`^#{1,6}\s+`)
	listItemRe = regexp.MustCompile(`^([-*+]|\d+[.)])\s+`)
)

// ParseDoc reads a document with optional front matter:
//
//	---
//	id: vacation-policy
//	title: Vacation policy
//	updated: 2026-03-01
//	groups: hr, managers
//	source: https://intranet/vacation
//	---
//
// Unknown keys are ignored so newer files still load.
func ParseDoc(name string, data []byte) (Doc, error) {
	text := strings.ToValidUTF8(string(data), "\uFFFD")
	text = strings.TrimPrefix(text, "\uFEFF")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	d := Doc{ID: strings.TrimSuffix(path.Base(name), path.Ext(name)), Source: name}

	if strings.HasPrefix(text, "---\n") {
		rest := text[len("---\n"):]
		var header string
		if strings.HasPrefix(rest, "---\n") || rest == "---" {
			rest = strings.TrimPrefix(strings.TrimPrefix(rest, "---"), "\n")
		} else {
			end := strings.Index(rest, "\n---")
			if end < 0 {
				return Doc{}, fmt.Errorf("%s: front matter is not closed with ---", name)
			}
			header = rest[:end]
			rest = strings.TrimPrefix(rest[end+len("\n---"):], "\n")
		}
		for n, line := range strings.Split(header, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				return Doc{}, fmt.Errorf("%s: front matter line %d has no key: %q", name, n+2, line)
			}
			k = strings.ToLower(strings.TrimSpace(k))
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			switch k {
			case "id":
				d.ID = v
			case "title":
				d.Title = v
			case "source":
				d.Source = v
			case "updated":
				t, err := time.Parse("2006-01-02", v)
				if err != nil {
					return Doc{}, fmt.Errorf("%s: updated must be YYYY-MM-DD: %w", name, err)
				}
				d.Updated = t
			case "supersedes":
				for _, s := range strings.Split(v, ",") {
					if s = strings.TrimSpace(s); s != "" {
						d.Supersedes = append(d.Supersedes, s)
					}
				}
			case "groups":
				for _, g := range strings.Split(v, ",") {
					if g = strings.TrimSpace(g); g != "" {
						d.Groups = append(d.Groups, g)
					}
				}
			}
		}
		text = rest
	}
	if strings.TrimSpace(d.ID) == "" {
		return Doc{}, fmt.Errorf("%s: empty document id", name)
	}
	if d.Title == "" {
		for _, line := range strings.Split(text, "\n") {
			if t := strings.TrimSpace(line); headingRe.MatchString(t) {
				d.Title = headingRe.ReplaceAllString(t, "")
				break
			}
		}
		if d.Title == "" {
			d.Title = d.ID
		}
	}
	d.Body = text
	return d, nil
}

// LoadDir loads every .md and .txt file under dir. Hidden directories and
// symlinks are skipped; duplicate ids are an error rather than a silent
// overwrite.
func LoadDir(dir string) ([]Doc, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	fsys := os.DirFS(dir)
	seen := map[string]string{}
	var docs []Doc
	err = fs.WalkDir(fsys, ".", func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if de.IsDir() {
			if p != "." && strings.HasPrefix(de.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if de.Type()&fs.ModeSymlink != 0 || !de.Type().IsRegular() {
			return nil
		}
		if ext := strings.ToLower(path.Ext(p)); ext != ".md" && ext != ".txt" {
			return nil
		}
		fi, err := de.Info()
		if err != nil {
			return err
		}
		if fi.Size() > MaxDocBytes {
			return fmt.Errorf("%s is %d bytes, over the %d byte limit", p, fi.Size(), MaxDocBytes)
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		d, err := ParseDoc(p, data)
		if err != nil {
			return err
		}
		if prev, dup := seen[d.ID]; dup {
			return fmt.Errorf("duplicate document id %q in %s and %s", d.ID, prev, p)
		}
		seen[d.ID] = p
		docs = append(docs, d)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, errors.New("no .md or .txt documents found in " + dir)
	}
	return docs, nil
}

// ChunkDoc splits a document on paragraph and list-item boundaries into
// chunks of at most maxWords words. A heading always starts a new chunk so
// each chunk's section is accurate. Each chunk is screened for injections.
func ChunkDoc(d Doc, maxWords int) []Chunk {
	if maxWords <= 0 {
		maxWords = 120
	}
	var (
		chunks  []Chunk
		section string
		paras   []string
		words   int
		cur     []string
	)
	flush := func() {
		if len(paras) == 0 {
			return
		}
		clean, removed := Screen(strings.Join(paras, "\n"))
		paras, words = nil, 0
		if strings.TrimSpace(clean) == "" {
			return // nothing left after screening; the model never sees it
		}
		header := d.Title
		if section != "" && !strings.EqualFold(section, d.Title) {
			header = d.Title + " — " + section
		}
		chunks = append(chunks, Chunk{
			DocID: d.ID, Title: d.Title, Section: section, Header: header, Source: d.Source,
			Updated: d.Updated, Groups: d.Groups, Text: clean, Removed: removed,
		})
	}
	add := func(p string) {
		n := len(strings.Fields(p))
		if n > maxWords {
			flush()
			for _, piece := range splitLong(p, maxWords) {
				paras = []string{piece}
				flush()
			}
			return
		}
		if words+n > maxWords {
			flush()
		}
		paras = append(paras, p)
		words += n
	}
	endPara := func() {
		if len(cur) > 0 {
			add(strings.Join(cur, " "))
			cur = nil
		}
	}
	for _, line := range strings.Split(d.Body, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "":
			endPara()
		case headingRe.MatchString(t):
			endPara()
			flush()
			section = strings.TrimSpace(headingRe.ReplaceAllString(t, ""))
		case listItemRe.MatchString(t):
			endPara()
			cur = []string{t}
		default:
			cur = append(cur, t)
		}
	}
	endPara()
	flush()
	for i := range chunks {
		chunks[i].ID = fmt.Sprintf("%s#%d", d.ID, i+1)
	}
	return chunks
}

// splitLong breaks an oversized paragraph at sentence boundaries, and a
// sentence that is itself too long at word boundaries.
func splitLong(p string, maxWords int) []string {
	var out, buf []string
	n := 0
	emit := func() {
		if len(buf) > 0 {
			out = append(out, strings.Join(buf, " "))
			buf, n = nil, 0
		}
	}
	for _, s := range Sentences(p) {
		w := strings.Fields(s)
		if len(w) > maxWords {
			emit()
			for len(w) > 0 {
				k := min(maxWords, len(w))
				out = append(out, strings.Join(w[:k], " "))
				w = w[k:]
			}
			continue
		}
		if n+len(w) > maxWords {
			emit()
		}
		buf = append(buf, s)
		n += len(w)
	}
	emit()
	return out
}
