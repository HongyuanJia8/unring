// Package audit persists the durable record of an unring session.
package audit

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/HongyuanJia8/unring/internal/httpsproxy"
	"github.com/HongyuanJia8/unring/internal/pgproxy"
)

const recordVersion = 1

// Approval records the user's answer to an irreversible-action prompt.
type Approval struct {
	Kind      string    `json:"kind"`
	Statement string    `json:"statement"`
	Reason    string    `json:"reason"`
	Decision  string    `json:"decision"`
	Error     string    `json:"error,omitempty"`
	Time      time.Time `json:"time"`
}

// Unintercepted records traffic for which unring could not provide coverage.
type Unintercepted struct {
	Kind      string    `json:"kind"`
	Host      string    `json:"host,omitempty"`
	Statement string    `json:"statement,omitempty"`
	Detail    string    `json:"detail"`
	Time      time.Time `json:"time"`
}

// Record is the structured, per-session audit document.
type Record struct {
	Version       int                `json:"version"`
	ID            string             `json:"id"`
	StartedAt     time.Time          `json:"started_at"`
	EndedAt       time.Time          `json:"ended_at,omitempty"`
	Command       []string           `json:"command"`
	Decision      string             `json:"decision"`
	Outcome       string             `json:"outcome"`
	ExitCode      int                `json:"exit_code"`
	Error         string             `json:"error,omitempty"`
	Postgres      pgproxy.Summary    `json:"postgres"`
	HTTPS         httpsproxy.Summary `json:"https"`
	Approvals     []Approval         `json:"irreversible_actions"`
	Unintercepted []Unintercepted    `json:"unintercepted"`
}

// Store owns the on-disk audit log beneath unring's per-user state directory.
type Store struct {
	stateDir string
	logDir   string
}

// OpenStore opens the default per-user store. UNRING_STATE_DIR is an explicit
// override intended for isolated installations and tests.
func OpenStore() (*Store, error) {
	return OpenStoreAt("")
}

// OpenStoreAt opens a store rooted at stateDir. An empty path selects the
// default per-user state directory.
func OpenStoreAt(stateDir string) (*Store, error) {
	if stateDir == "" {
		var err error
		stateDir, err = StateDir()
		if err != nil {
			return nil, err
		}
	}
	stateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, fmt.Errorf("resolve unring state directory: %w", err)
	}
	logDir := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, fmt.Errorf("create unring audit directory: %w", err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("restrict unring state directory: %w", err)
	}
	if err := os.Chmod(logDir, 0o700); err != nil {
		return nil, fmt.Errorf("restrict unring audit directory: %w", err)
	}
	return &Store{stateDir: stateDir, logDir: logDir}, nil
}

// StateDir returns unring's only per-user state root.
func StateDir() (string, error) {
	if override := os.Getenv("UNRING_STATE_DIR"); override != "" {
		return override, nil
	}
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "unring"), nil
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find per-user state directory: %w", err)
	}
	return filepath.Join(configDir, "unring"), nil
}

// StateDir returns this store's state root.
func (s *Store) StateDir() string {
	return s.stateDir
}

// NewRecord creates the initial durable state for a run.
func NewRecord(command []string, now time.Time) (Record, error) {
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Record{}, fmt.Errorf("generate audit session id: %w", err)
	}
	id := now.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(random[:])
	return Record{
		Version:   recordVersion,
		ID:        id,
		StartedAt: now.UTC(),
		Command:   append([]string(nil), command...),
		Decision:  "discard",
		Outcome:   "pending",
		Postgres: pgproxy.Summary{
			FullyReversible: true,
			Changes:         pgproxy.ChangeSummary{Complete: false, Error: "session not sealed"},
		},
	}, nil
}

// Save atomically replaces a session record with restrictive permissions.
func (s *Store) Save(record Record) error {
	if record.ID == "" || strings.ContainsAny(record.ID, `/\`) {
		return errors.New("save audit record: invalid session id")
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode audit record: %w", err)
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(s.logDir, ".audit-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary audit record: %w", err)
	}
	tempName := temp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempName)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("restrict temporary audit record: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary audit record: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync temporary audit record: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary audit record: %w", err)
	}
	path := filepath.Join(s.logDir, record.ID+".json")
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("publish audit record: %w", err)
	}
	removeTemp = false
	return nil
}

// List returns all readable records, newest first.
func (s *Store) List() ([]Record, error) {
	entries, err := os.ReadDir(s.logDir)
	if err != nil {
		return nil, fmt.Errorf("list audit records: %w", err)
	}
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		record, err := s.loadPath(filepath.Join(s.logDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].StartedAt.After(records[j].StartedAt)
	})
	return records, nil
}

// Load finds an exact session id or an unambiguous id prefix.
func (s *Store) Load(id string) (Record, error) {
	if id == "" || strings.ContainsAny(id, `/\`) {
		return Record{}, errors.New("load audit record: invalid session id")
	}
	exact := filepath.Join(s.logDir, id+".json")
	if _, err := os.Stat(exact); err == nil {
		return s.loadPath(exact)
	}
	entries, err := os.ReadDir(s.logDir)
	if err != nil {
		return Record{}, fmt.Errorf("list audit records: %w", err)
	}
	var match string
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".json")
		if filepath.Ext(entry.Name()) == ".json" && strings.HasPrefix(name, id) {
			if match != "" {
				return Record{}, fmt.Errorf("audit session prefix %q is ambiguous", id)
			}
			match = filepath.Join(s.logDir, entry.Name())
		}
	}
	if match == "" {
		return Record{}, fmt.Errorf("audit session %q not found", id)
	}
	return s.loadPath(match)
}

func (s *Store) loadPath(path string) (Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, fmt.Errorf("read audit record %s: %w", filepath.Base(path), err)
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return Record{}, fmt.Errorf("decode audit record %s: %w", filepath.Base(path), err)
	}
	return record, nil
}

// Session synchronizes incremental updates to one crash-oriented audit record.
type Session struct {
	mu     sync.Mutex
	store  *Store
	record Record
}

// Begin writes the initial record before the wrapped child starts.
func (s *Store) Begin(command []string, now time.Time) (*Session, error) {
	record, err := NewRecord(command, now)
	if err != nil {
		return nil, err
	}
	session := &Session{store: s, record: record}
	if err := s.Save(record); err != nil {
		return nil, err
	}
	return session, nil
}

// Update changes and immediately persists the record.
func (s *Session) Update(change func(*Record)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(&s.record)
	return s.store.Save(s.record)
}

// Snapshot returns a detached copy of the current record.
func (s *Session) Snapshot() Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.Marshal(s.record)
	var copy Record
	_ = json.Unmarshal(data, &copy)
	return copy
}
