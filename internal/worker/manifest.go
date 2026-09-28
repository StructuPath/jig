// manifest.go — per-attempt protected manifests and the disposal journal
// (U3), the factory:manifest.go shape simplified for jig: atomic 0600 JSON
// files under <data>/attempts/, validated on every read and write, plus a
// grow-only journal of attempts whose worktrees were provably disposed. The
// manifest is the worker's durable memory: reconciliation trusts it over
// anything else on disk, and cleanup refuses to touch a worktree whose
// manifest it cannot read and validate (fail closed, R16).
package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const manifestSchemaVersion = 1
const disposalJournalSchemaVersion = 1

// Manifest lifecycle states. Forward motion is
// preparing → worktree_created → running → completed, then exactly one of
// cleaned (proof-of-publish or clean-at-base deletion), retained
// (fail-closed keep), or missing (disk vanished outside our control).
// not_created records a preparation that died before the worktree existed.
const (
	manifestPreparing       = "preparing"
	manifestWorktreeCreated = "worktree_created"
	manifestRunning         = "running"
	manifestCompleted       = "completed"
	manifestRetained        = "retained"
	manifestCleanupStarted  = "cleanup_started"
	manifestCleaned         = "cleaned"
	manifestNotCreated      = "not_created"
	manifestMissing         = "missing"
)

// Cleanup intents (R16): automatic requires proof, operator_confirmed rides
// the ledger release action.
const (
	cleanupIntentAutomatic = "automatic"
	cleanupIntentOperator  = "operator_confirmed"
)

var manifestLifecycles = map[string]bool{
	manifestPreparing: true, manifestWorktreeCreated: true, manifestRunning: true,
	manifestCompleted: true, manifestRetained: true, manifestCleanupStarted: true,
	manifestCleaned: true, manifestNotCreated: true, manifestMissing: true,
}

// attemptManifest is the durable record of one attempt's materialized state:
// repository identity, worktree path, branch, pinned base SHA. Process
// identity fields join in U4 when agent subprocesses exist.
type attemptManifest struct {
	SchemaVersion int `json:"schema_version"`

	WorkerID      string `json:"worker_id"`
	JobID         string `json:"job_id"`
	AttemptID     string `json:"attempt_id"`
	AttemptNumber int    `json:"attempt_number"`

	Repository    string `json:"repository"`
	RepositoryDir string `json:"repository_dir"`
	BaseSHA       string `json:"base_sha"`
	WorktreePath  string `json:"worktree_path"`
	Branch        string `json:"branch"`

	// ProcessGroups is the SET of agent process groups live in this attempt
	// right now. A parallel reviewer group runs several agent subprocesses at
	// once, each in its own group, and start-time reconciliation must stop
	// every one a crashed worker left behind.
	ProcessGroups []int64 `json:"process_groups,omitempty"`
	// ProcessGroupID and ProcessActive are the single-group record older jig
	// versions wrote. They are still read — reconciliation stops a group a
	// crashed older worker recorded — but no longer written.
	ProcessGroupID int64 `json:"process_group_id,omitempty"`
	ProcessActive  bool  `json:"process_active"`

	Lifecycle       string    `json:"lifecycle"`
	TerminalState   string    `json:"terminal_state,omitempty"`
	RetentionReason string    `json:"retention_reason,omitempty"`
	CleanupIntent   string    `json:"cleanup_intent,omitempty"`
	CleanupResult   string    `json:"cleanup_result,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type disposalJournal struct {
	SchemaVersion int      `json:"schema_version"`
	WorkerID      string   `json:"worker_id"`
	AttemptIDs    []string `json:"attempt_ids"`
}

// manifestStore owns the attempts directory and the disposal journal under
// one worker data directory.
type manifestStore struct {
	dataDirectory string
	workerID      string
	now           func() time.Time
	mutex         sync.Mutex
}

func newManifestStore(dataDirectory, workerID string) *manifestStore {
	return &manifestStore{dataDirectory: dataDirectory, workerID: workerID, now: time.Now}
}

func (store *manifestStore) attemptsDirectory() string {
	return filepath.Join(store.dataDirectory, "attempts")
}

func (store *manifestStore) path(attemptID string) (string, error) {
	if !uuidPattern.MatchString(attemptID) {
		return "", errors.New("invalid attempt ID")
	}
	return filepath.Join(store.attemptsDirectory(), attemptID+".json"), nil
}

func (store *manifestStore) create(manifest attemptManifest) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	path, err := store.path(manifest.AttemptID)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("attempt manifest already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect attempt manifest: %w", err)
	}
	now := store.now().UTC()
	manifest.SchemaVersion = manifestSchemaVersion
	manifest.WorkerID = store.workerID
	manifest.CreatedAt = now
	manifest.UpdatedAt = now
	if err := store.validate(manifest); err != nil {
		return err
	}
	return store.writeLocked(path, manifest)
}

func (store *manifestStore) update(attemptID string, change func(*attemptManifest) error) (attemptManifest, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	path, err := store.path(attemptID)
	if err != nil {
		return attemptManifest{}, err
	}
	manifest, err := store.readLocked(path)
	if err != nil {
		return attemptManifest{}, err
	}
	if err := change(&manifest); err != nil {
		return attemptManifest{}, err
	}
	manifest.UpdatedAt = store.now().UTC()
	if err := store.validate(manifest); err != nil {
		return attemptManifest{}, err
	}
	if err := store.writeLocked(path, manifest); err != nil {
		return attemptManifest{}, err
	}
	return manifest, nil
}

func (store *manifestStore) load(attemptID string) (attemptManifest, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	path, err := store.path(attemptID)
	if err != nil {
		return attemptManifest{}, err
	}
	return store.readLocked(path)
}

// loadAll reads every manifest in the attempts directory, removing stale
// temporary files from interrupted writes. Unreadable manifests are joined
// into the returned error; the readable rest still load, so one corrupt
// file cannot blind reconciliation to everything else.
func (store *manifestStore) loadAll() ([]attemptManifest, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	directory, err := store.ensureDirectory()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read attempt manifests: %w", err)
	}
	manifests := make([]attemptManifest, 0, len(entries))
	var loadErrors []error
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") && strings.HasSuffix(entry.Name(), ".tmp") {
			if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil {
				loadErrors = append(loadErrors, fmt.Errorf("remove stale temporary manifest: %w", err))
			}
			continue
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			loadErrors = append(loadErrors,
				fmt.Errorf("unexpected entry in attempt manifest directory: %s", entry.Name()))
			continue
		}
		path, pathErr := store.path(strings.TrimSuffix(entry.Name(), ".json"))
		if pathErr != nil {
			loadErrors = append(loadErrors, fmt.Errorf("invalid attempt manifest filename %q", entry.Name()))
			continue
		}
		manifest, readErr := store.readLocked(path)
		if readErr != nil {
			loadErrors = append(loadErrors, fmt.Errorf("%s: %w", entry.Name(), readErr))
			continue
		}
		manifests = append(manifests, manifest)
	}
	sort.Slice(manifests, func(i, j int) bool {
		return manifests[i].AttemptID < manifests[j].AttemptID
	})
	return manifests, errors.Join(loadErrors...)
}

// ---- disposal journal ------------------------------------------------------
//
// The journal lists attempts whose worktrees were provably disposed —
// deleted after remote-ref proof or an operator release. Reconciliation
// skips journaled attempts, so a leftover manifest can never resurrect a
// disposed worktree's ledger row.

func (store *manifestStore) disposalJournalPath() string {
	return filepath.Join(store.dataDirectory, "disposed-attempts.json")
}

func (store *manifestStore) loadDisposals() ([]string, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	journal, err := store.readDisposalsLocked()
	if err != nil {
		return nil, err
	}
	return append([]string(nil), journal.AttemptIDs...), nil
}

func (store *manifestStore) addDisposal(attemptID string) error {
	if !uuidPattern.MatchString(attemptID) {
		return errors.New("invalid disposed attempt ID")
	}
	store.mutex.Lock()
	defer store.mutex.Unlock()
	journal, err := store.readDisposalsLocked()
	if err != nil {
		return err
	}
	for _, existing := range journal.AttemptIDs {
		if existing == attemptID {
			return nil
		}
	}
	journal.AttemptIDs = append(journal.AttemptIDs, attemptID)
	sort.Strings(journal.AttemptIDs)
	return store.writeProtectedJSON(store.disposalJournalPath(), journal)
}

func (store *manifestStore) readDisposalsLocked() (disposalJournal, error) {
	journal := disposalJournal{
		SchemaVersion: disposalJournalSchemaVersion,
		WorkerID:      store.workerID,
		AttemptIDs:    []string{},
	}
	path := store.disposalJournalPath()
	body, err := readProtectedFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return journal, nil
	}
	if err != nil {
		return disposalJournal{}, fmt.Errorf("read disposal journal: %w", err)
	}
	if err := decodeStrictJSON(body, &journal); err != nil {
		return disposalJournal{}, fmt.Errorf("decode disposal journal: %w", err)
	}
	if journal.SchemaVersion != disposalJournalSchemaVersion {
		return disposalJournal{}, fmt.Errorf(
			"unsupported disposal journal schema version %d", journal.SchemaVersion)
	}
	if journal.WorkerID != store.workerID {
		return disposalJournal{}, errors.New("disposal journal belongs to a different worker")
	}
	seen := make(map[string]bool, len(journal.AttemptIDs))
	for _, attemptID := range journal.AttemptIDs {
		if !uuidPattern.MatchString(attemptID) || seen[attemptID] {
			return disposalJournal{}, errors.New("disposal journal attempt IDs must be unique UUIDs")
		}
		seen[attemptID] = true
	}
	sort.Strings(journal.AttemptIDs)
	return journal, nil
}

// ---- protected file primitives ---------------------------------------------

func (store *manifestStore) readLocked(path string) (attemptManifest, error) {
	body, err := readProtectedFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return attemptManifest{}, errors.New("attempt manifest does not exist")
	}
	if err != nil {
		return attemptManifest{}, fmt.Errorf("read attempt manifest: %w", err)
	}
	var manifest attemptManifest
	if err := decodeStrictJSON(body, &manifest); err != nil {
		return attemptManifest{}, fmt.Errorf("decode attempt manifest: %w", err)
	}
	if err := store.validate(manifest); err != nil {
		return attemptManifest{}, err
	}
	return manifest, nil
}

func (store *manifestStore) writeLocked(path string, manifest attemptManifest) error {
	if _, err := store.ensureDirectory(); err != nil {
		return err
	}
	return store.writeProtectedJSON(path, manifest)
}

func (store *manifestStore) ensureDirectory() (string, error) {
	directory := store.attemptsDirectory()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create attempt manifest directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return "", fmt.Errorf("inspect attempt manifest directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("attempt manifest directory must be a real directory, not a symlink")
	}
	return directory, nil
}

// writeProtectedJSON writes a 0600 JSON file atomically: temp file in the
// same directory, fsync, rename over the target.
func (store *manifestStore) writeProtectedJSON(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	body = append(body, '\n')
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", filepath.Base(path), err)
	}
	temporary := file.Name()
	removeTemporary := true
	defer func() {
		_ = file.Close()
		if removeTemporary {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("protect %s: %w", filepath.Base(path), err)
	}
	if _, err := file.Write(body); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", filepath.Base(path), err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("replace %s: %w", filepath.Base(path), err)
	}
	removeTemporary = false
	return nil
}

// readProtectedFile refuses symlinks, non-regular files, and group/other
// access before reading — a protected file another user can write is not a
// record, it is an attack surface.
func readProtectedFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("must be a regular non-symlink file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("permissions must not allow group or other access")
	}
	return os.ReadFile(path)
}

func decodeStrictJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON content")
	}
	return nil
}

func (store *manifestStore) validate(manifest attemptManifest) error {
	if manifest.SchemaVersion != manifestSchemaVersion {
		return fmt.Errorf("unsupported attempt manifest schema version %d", manifest.SchemaVersion)
	}
	for name, value := range map[string]string{
		"worker_id": manifest.WorkerID, "job_id": manifest.JobID, "attempt_id": manifest.AttemptID,
	} {
		if !uuidPattern.MatchString(value) {
			return fmt.Errorf("attempt manifest %s is not a valid UUID", name)
		}
	}
	if manifest.WorkerID != store.workerID {
		return errors.New("attempt manifest belongs to a different worker")
	}
	if manifest.AttemptNumber < 1 {
		return errors.New("attempt manifest attempt number must be positive")
	}
	if strings.TrimSpace(manifest.Repository) == "" {
		return errors.New("attempt manifest repository identity is required")
	}
	if !filepath.IsAbs(manifest.RepositoryDir) || filepath.Clean(manifest.RepositoryDir) != manifest.RepositoryDir {
		return errors.New("attempt manifest repository directory is not canonical")
	}
	if !commitPattern.MatchString(manifest.BaseSHA) {
		return errors.New("attempt manifest base SHA is invalid")
	}
	expectedPath := filepath.Join(store.dataDirectory, "worktrees", manifest.AttemptID)
	if manifest.WorktreePath != expectedPath {
		return errors.New("attempt manifest worktree path is not the owned jig path")
	}
	if manifest.Branch != attemptBranch(manifest.JobID, manifest.AttemptNumber) {
		return errors.New("attempt manifest branch does not match its job and attempt")
	}
	// The process group is a signal target, so it is validated as one on every
	// read and write: reconciliation negates this number and hands it to
	// kill(2), where 1 means "every process this user may signal" and 0 means
	// "jig's own group". Zero alone is the unrecorded state; anything else
	// below minimumSignallableProcessGroup is corruption, and a manifest that
	// advertises a live process must carry a group id that can name one.
	if manifest.ProcessGroupID != 0 && !signallableProcessGroup(manifest.ProcessGroupID) {
		return fmt.Errorf("attempt manifest process group %d can never name a real group",
			manifest.ProcessGroupID)
	}
	if manifest.ProcessActive && !signallableProcessGroup(manifest.ProcessGroupID) {
		return errors.New("attempt manifest advertises a live process without a signallable group id")
	}
	seenGroups := make(map[int64]bool, len(manifest.ProcessGroups))
	for _, groupID := range manifest.ProcessGroups {
		if !signallableProcessGroup(groupID) {
			return fmt.Errorf("attempt manifest process group %d can never name a real group", groupID)
		}
		if seenGroups[groupID] {
			return fmt.Errorf("attempt manifest lists process group %d twice", groupID)
		}
		seenGroups[groupID] = true
	}
	if !manifestLifecycles[manifest.Lifecycle] {
		return fmt.Errorf("attempt manifest lifecycle %q is invalid", manifest.Lifecycle)
	}
	if manifest.CleanupIntent != "" &&
		manifest.CleanupIntent != cleanupIntentAutomatic &&
		manifest.CleanupIntent != cleanupIntentOperator {
		return fmt.Errorf("attempt manifest cleanup intent %q is invalid", manifest.CleanupIntent)
	}
	if manifest.CreatedAt.IsZero() || manifest.UpdatedAt.IsZero() {
		return errors.New("attempt manifest timestamps are incomplete")
	}
	return nil
}

func boundedText(value string, limit int) string {
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
