// Package brain is Samrajya's memory of the work it does: short notes on
// things worth knowing next time - a person's standing instructions, facts
// that are neither in the code nor on Linear, problems that keep coming
// back. Senapati keeps it (through the HTTP API in this package); it is
// summarised in every Senapati briefing, so it survives rotations and
// restarts, and the notes for a project are given to every sainik started
// in that project.
//
// Notes are plain markdown files a person can read, edit or delete:
//
//	brain/<name>.md     ---\ndescription: ...\nproject: ...\nupdated: ...\n---\n<body>
//	brain/INDEX.md      one line per note, rewritten on every change
//
// It is kept deliberately small: short notes and a cap on how many, so it
// holds what matters instead of everything.
package brain

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	MaxNotes       = 50
	MaxBody        = 1500 // bytes
	MaxDescription = 200
)

var (
	ErrNotFound = errors.New("no such note")
	ErrInvalid  = errors.New("invalid note")
	ErrFull     = errors.New("memory is full")
)

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,59}$`)

// Note is one thing worth remembering.
type Note struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Project     string    `json:"project,omitempty"` // a workspace project; empty = applies everywhere
	Body        string    `json:"body,omitempty"`
	Updated     time.Time `json:"updated"`
}

// Store is the brain directory.
type Store struct {
	mu  sync.Mutex
	dir string
	// projects lists the workspace's projects, to check a note's project
	// against; nil means any project name is accepted.
	projects func() []string
}

// Open uses dir, creating it if needed.
func Open(dir string, projects func() []string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir, projects: projects}, nil
}

// List returns every note without its body, newest first.
func (s *Store) List() ([]Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	notes, err := s.readAll()
	for i := range notes {
		notes[i].Body = ""
	}
	return notes, err
}

// Get returns one note.
func (s *Store) Get(name string) (Note, error) {
	if !validName.MatchString(name) {
		return Note{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read(name)
}

// ForProject returns the full notes for project, oldest first.
func (s *Store) ForProject(project string) []Note {
	if project == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	notes, _ := s.readAll()
	var out []Note
	for _, n := range notes {
		if n.Project == project {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.Before(out[j].Updated) })
	return out
}

// Put creates or replaces a note. It reports whether the note is new.
func (s *Store) Put(n Note) (created bool, err error) {
	n.Description = strings.TrimSpace(n.Description)
	n.Project = strings.TrimSpace(n.Project)
	n.Body = strings.TrimSpace(n.Body)
	switch {
	case !validName.MatchString(n.Name):
		return false, fmt.Errorf("%w: name %q must be lowercase letters, digits and dashes, at most 60", ErrInvalid, n.Name)
	case n.Description == "" || strings.ContainsAny(n.Description, "\n\r"):
		return false, fmt.Errorf("%w: description must be one line saying what the note is about", ErrInvalid)
	case len(n.Description) > MaxDescription:
		return false, fmt.Errorf("%w: description is %d characters; keep it under %d", ErrInvalid, len(n.Description), MaxDescription)
	case n.Body == "":
		return false, fmt.Errorf("%w: body is empty", ErrInvalid)
	case len(n.Body) > MaxBody:
		return false, fmt.Errorf("%w: body is %d bytes; a note must stay under %d - keep only what matters next time", ErrInvalid, len(n.Body), MaxBody)
	}
	if n.Project != "" && s.projects != nil {
		known := s.projects()
		found := false
		for _, p := range known {
			found = found || p == n.Project
		}
		if !found {
			return false, fmt.Errorf("%w: project %q is not in the workspace (projects: %s); leave it empty for a note that applies everywhere",
				ErrInvalid, n.Project, strings.Join(known, ", "))
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = os.Stat(s.path(n.Name))
	created = errors.Is(err, os.ErrNotExist)
	if created {
		notes, _ := s.readAll()
		if len(notes) >= MaxNotes {
			return false, fmt.Errorf("%w: it holds %d notes, the most it keeps. Merge related notes or delete ones that no longer matter, then add this",
				ErrFull, len(notes))
		}
	}
	n.Updated = time.Now().UTC()
	if err := writeFile(s.path(n.Name), render(n)); err != nil {
		return false, err
	}
	return created, s.writeIndex()
}

// Delete removes a note.
func (s *Store) Delete(name string) error {
	if !validName.MatchString(name) {
		return ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(name)); errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	return s.writeIndex()
}

// Index is the notes as Senapati's briefing shows them: one line each.
func (s *Store) Index() string {
	notes, err := s.List()
	if err != nil || len(notes) == 0 {
		return ""
	}
	return indexLines(notes)
}

func indexLines(notes []Note) string {
	sort.Slice(notes, func(i, j int) bool {
		if notes[i].Project != notes[j].Project {
			return notes[i].Project < notes[j].Project
		}
		return notes[i].Name < notes[j].Name
	})
	var b strings.Builder
	for _, n := range notes {
		where := "everywhere"
		if n.Project != "" {
			where = "project " + n.Project
		}
		fmt.Fprintf(&b, "- %s (%s): %s\n", n.Name, where, n.Description)
	}
	return b.String()
}

func (s *Store) path(name string) string { return filepath.Join(s.dir, name+".md") }

func (s *Store) read(name string) (Note, error) {
	data, err := os.ReadFile(s.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return Note{}, ErrNotFound
	}
	if err != nil {
		return Note{}, err
	}
	n := parse(name, string(data))
	if n.Updated.IsZero() { // written by hand, without the header
		if st, err := os.Stat(s.path(name)); err == nil {
			n.Updated = st.ModTime().UTC()
		}
	}
	return n, nil
}

func (s *Store) readAll() ([]Note, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []Note
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".md")
		if !ok || e.IsDir() || !validName.MatchString(name) {
			continue // INDEX.md and anything a person keeps there
		}
		if n, err := s.read(name); err == nil {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

func (s *Store) writeIndex() error {
	notes, err := s.readAll()
	if err != nil {
		return err
	}
	text := "# Samrajya's memory\n\nWritten by mahamantri; edit the notes, not this file.\n\n"
	if len(notes) == 0 {
		text += "(no notes)\n"
	} else {
		text += indexLines(notes)
	}
	return writeFile(filepath.Join(s.dir, "INDEX.md"), text)
}

func render(n Note) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "description: %s\n", n.Description)
	if n.Project != "" {
		fmt.Fprintf(&b, "project: %s\n", n.Project)
	}
	fmt.Fprintf(&b, "updated: %s\n", n.Updated.Format(time.RFC3339))
	b.WriteString("---\n\n")
	b.WriteString(n.Body)
	b.WriteString("\n")
	return b.String()
}

// parse reads a note file. A file a person wrote without frontmatter is
// still a note: its first line is its description.
func parse(name, text string) Note {
	n := Note{Name: name}
	if rest, ok := strings.CutPrefix(text, "---\n"); ok {
		if head, body, found := strings.Cut(rest, "\n---\n"); found {
			for _, line := range strings.Split(head, "\n") {
				key, value, _ := strings.Cut(line, ":")
				value = strings.TrimSpace(value)
				switch strings.TrimSpace(key) {
				case "description":
					n.Description = value
				case "project":
					n.Project = value
				case "updated":
					n.Updated, _ = time.Parse(time.RFC3339, value)
				}
			}
			n.Body = strings.TrimSpace(body)
			return n
		}
	}
	n.Body = strings.TrimSpace(text)
	first, _, _ := strings.Cut(n.Body, "\n")
	n.Description = strings.TrimSpace(strings.TrimLeft(first, "# "))
	return n
}

func writeFile(path, text string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
