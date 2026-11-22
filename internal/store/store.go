// Package store persists sessions as JSON files.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"hearth/internal/core"
)

// Store manages session files in a directory.
type Store struct {
	dir string
}

// New creates the sessions directory if needed.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// Save atomically writes a session file. Marshals from a lock-protected
// clone so concurrent agent mutations can't tear the output.
func (s *Store) Save(sess *core.Session) error {
	b, err := json.MarshalIndent(sess.Clone(), "", "  ")
	if err != nil {
		return err
	}
	p := s.path(sess.ID)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Load reads one session by id.
func (s *Store) Load(id string) (*core.Session, error) {
	if strings.ContainsAny(id, "/\\") || id == "." || id == ".." {
		return nil, fmt.Errorf("invalid session id")
	}
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, err
	}
	var sess core.Session
	if err := json.Unmarshal(b, &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

// Delete removes a session file (used by the delete flow).
func (s *Store) Delete(id string) error {
	return os.Remove(s.path(id))
}

// SessionMeta is list view data for a session.
type SessionMeta struct {
	ID        string
	Title     string
	Model     string
	MsgCount  int
	UpdatedAt time.Time
}

// List scans the session directory, newest first.
func (s *Store) List() ([]SessionMeta, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []SessionMeta
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			continue
		}
		var sess core.Session
		if err := json.Unmarshal(b, &sess); err != nil {
			continue
		}
		out = append(out, SessionMeta{
			ID:        sess.ID,
			Title:     sess.Title,
			Model:     sess.Model,
			MsgCount:  len(sess.Messages),
			UpdatedAt: sess.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// Latest returns the most recently updated session, if any.
func (s *Store) Latest() (*core.Session, error) {
	list, err := s.List()
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return s.Load(list[0].ID)
}
