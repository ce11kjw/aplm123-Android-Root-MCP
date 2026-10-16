// Package knowledge provides progressively loaded built-in skills and a
// privacy-preserving, crash-recoverable execution memory for completed MCP
// calls.
package knowledge

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aplm123/android-ai-mcp/internal/atomicfile"
	"github.com/aplm123/android-ai-mcp/internal/model"
)

const (
	learnedEntryVersion           = 4
	maxReceiptFiles               = 20000
	receiptLowWaterFiles          = 16000
	maxReceiptBytes         int64 = 64 << 20
	receiptLowWaterBytes          = 48 << 20
	compactedTaskMagic            = "ZCRTKID1"
	forgetStateVersion            = 1
	indexStateVersion             = 1
	maxSkillBodyBytes             = 64 << 10
	maxKnowledgeIndexBytes        = 8 << 20
	maxForgetStateBytes           = 1 << 20
	maxReceiptFileBytes           = 128 << 10
	maxLearnedArtifactTypes       = 16
	knowledgeStateMarker          = "aplm123-KNOWLEDGE-STATE-V1\n"
)

var safeIdentifier = regexp.MustCompile(`^[a-z0-9_][a-z0-9_.-]{0,95}$`)
var numericHost = regexp.MustCompile(`^[0-9.]+$`)

//go:embed builtin/*.md
var builtinFS embed.FS

type Entry struct {
	Version                int            `json:"version,omitempty"`
	ID                     string         `json:"id"`
	Title                  string         `json:"title"`
	Summary                string         `json:"summary"`
	Keywords               []string       `json:"keywords,omitempty"`
	Triggers               []string       `json:"triggers,omitempty"`
	Source                 string         `json:"source"`
	Tool                   string         `json:"tool,omitempty"`
	Action                 string         `json:"action,omitempty"`
	UpdatedAt              time.Time      `json:"updatedAt,omitempty"`
	Attempts               int            `json:"attempts,omitempty"`
	Successes              int            `json:"successes,omitempty"`
	Recoveries             int            `json:"recoveries,omitempty"`
	FailureClasses         map[string]int `json:"failureClasses,omitempty"`
	LastSuccessAt          time.Time      `json:"lastSuccessAt,omitempty"`
	LastFailureAt          time.Time      `json:"lastFailureAt,omitempty"`
	LastSuccessOrder       uint64         `json:"lastSuccessOrder,omitempty"`
	LastFailureOrder       uint64         `json:"lastFailureOrder,omitempty"`
	LastCode               string         `json:"lastCode,omitempty"`
	LastStrategy           string         `json:"lastStrategy,omitempty"`
	LastErrorClass         string         `json:"lastErrorClass,omitempty"`
	LastDurationMS         int64          `json:"lastDurationMs,omitempty"`
	LastExitCode           int            `json:"lastExitCode,omitempty"`
	LastFingerprint        string         `json:"lastFingerprint,omitempty"`
	LastOrder              uint64         `json:"lastOrder,omitempty"`
	LastArguments          map[string]any `json:"lastArguments,omitempty"`
	LastKnownGoodAt        time.Time      `json:"lastKnownGoodAt,omitempty"`
	LastKnownGoodCode      string         `json:"lastKnownGoodCode,omitempty"`
	LastKnownGoodStrategy  string         `json:"lastKnownGoodStrategy,omitempty"`
	LastKnownGoodArguments map[string]any `json:"lastKnownGoodArguments,omitempty"`
	LastRebootRequired     bool           `json:"lastRebootRequired,omitempty"`
	LastArtifacts          []string       `json:"lastArtifacts,omitempty"`
	LastKnownGoodReboot    bool           `json:"lastKnownGoodRebootRequired,omitempty"`
	LastKnownGoodArtifacts []string       `json:"lastKnownGoodArtifacts,omitempty"`
	ProcessedReceipts      []string       `json:"processedReceipts,omitempty"`
	Generation             uint64         `json:"generation,omitempty"`
}

type Match struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	Source  string `json:"source"`
	Score   int    `json:"score"`
}

type IndexItem struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Summary   string    `json:"summary"`
	Source    string    `json:"source"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

type receipt struct {
	Version     int            `json:"version"`
	ID          string         `json:"id"`
	CompletedAt time.Time      `json:"completedAt"`
	TaskID      string         `json:"taskId,omitempty"`
	Tool        string         `json:"tool"`
	Action      string         `json:"action"`
	Fingerprint string         `json:"fingerprint"`
	Generation  uint64         `json:"generation,omitempty"`
	Order       uint64         `json:"order,omitempty"`
	Arguments   map[string]any `json:"arguments"`
	Outcome     outcome        `json:"outcome"`
}

type outcome struct {
	Success        bool     `json:"success"`
	Code           string   `json:"code"`
	Strategy       string   `json:"strategy"`
	DurationMS     int64    `json:"durationMs"`
	ExitCode       int      `json:"exitCode"`
	RebootRequired bool     `json:"rebootRequired"`
	ErrorClass     string   `json:"errorClass,omitempty"`
	ErrorDigest    string   `json:"errorDigest,omitempty"`
	Artifacts      []string `json:"artifacts,omitempty"`
}

type receiptFile struct {
	path  string
	size  int64
	data  receipt
	valid bool
}

type forgetMarker struct {
	Generation uint64    `json:"generation"`
	At         time.Time `json:"at"`
}

type forgetState struct {
	Version  int                     `json:"version"`
	Markers  map[string]forgetMarker `json:"markers"`
	Checksum string                  `json:"checksum"`
}

type indexState struct {
	Version  int     `json:"version"`
	Entries  []Entry `json:"entries"`
	Checksum string  `json:"checksum"`
}

type Store struct {
	mu            sync.RWMutex
	root          string
	mirror        string
	builtins      map[string]string
	index         map[string]Entry
	forgotten     map[string]forgetMarker
	taskIDs       map[string]struct{}
	taskReceipts  map[string]receipt
	compactedIDs  map[[32]byte]struct{}
	ledgerCorrupt bool
	forgetCorrupt bool
	indexCorrupt  bool
	formatCurrent bool
	formatCorrupt bool
	receipts      int
	receiptBytes  int64
	nextOrder     uint64
	lastError     string
}

func New(stateDir, workDir string) *Store {
	s := &Store{
		root:         filepath.Join(stateDir, "MCP"),
		mirror:       filepath.Join(workDir, "MCP", "skills"),
		builtins:     make(map[string]string),
		index:        make(map[string]Entry),
		forgotten:    make(map[string]forgetMarker),
		taskIDs:      make(map[string]struct{}),
		taskReceipts: make(map[string]receipt),
		compactedIDs: make(map[[32]byte]struct{}),
	}
	s.loadBuiltins()
	s.loadIndex()
	return s
}

var builtinTriggerCatalog = map[string][]string{
	"builtin/android-web-automation": {
		"aplm_network", "web_extract", "browser_capture", "feed_parse", "cookie", "websocket", "rss", "atom", "网页截图", "内容提取",
	},
	"builtin/mcp-efficient-workflow": {
		"aplm_status", "aplm_config", "aplm_task", "aplm_transfer_upload", "aplm_transfer_export", "aplm_download", "aplm_archive", "aplm_backup", "aplm_schedule", "aplm_log", "aplm_diagnostics", "aplm_fs_info", "aplm_fs_read", "aplm_fs_write", "aplm_fs_manage", "aplm_fs_search", "aplm_fs_hash", "aplm_shell", "aplm_script", "mcp", "token", "后台任务", "渐进加载",
	},
}

func (s *Store) loadBuiltins() {
	items, _ := fs.Glob(builtinFS, "builtin/*.md")
	for _, path := range items {
		raw, err := builtinFS.ReadFile(path)
		if err != nil {
			continue
		}
		id := "builtin/" + strings.TrimSuffix(filepath.Base(path), ".md")
		body := string(raw)
		title := firstHeading(body)
		triggers := append([]string(nil), builtinTriggerCatalog[id]...)
		entry := Entry{Version: learnedEntryVersion, ID: id, Title: title, Summary: firstParagraph(body), Keywords: splitWords(id + " " + title + " " + body), Triggers: triggers, Source: "builtin"}
		s.builtins[id] = body
		s.index[id] = entry
	}
}

func (s *Store) loadIndex() {
	s.loadStateFormat()
	indexPath := filepath.Join(s.root, "skills", "index.json")
	learned, primaryLegacy, primaryErr := readKnowledgeIndex(indexPath)
	if primaryErr == nil && primaryLegacy && s.formatCurrent {
		primaryErr = errors.New("learned skill index format downgrade refused")
	}
	if primaryErr != nil {
		var backupErr error
		var backupLegacy bool
		learned, backupLegacy, backupErr = readKnowledgeIndex(indexPath + ".bak")
		if backupErr == nil && backupLegacy && s.formatCurrent {
			backupErr = errors.New("learned skill index backup format downgrade refused")
		}
		if backupErr != nil {
			if s.formatCurrent || !os.IsNotExist(primaryErr) || !os.IsNotExist(backupErr) {
				s.indexCorrupt = true
				s.rememberError(fmt.Errorf("learned skill index is untrusted: primary=%v; backup=%v", primaryErr, backupErr))
			}
			learned = nil
		} else {
			s.rememberError(fmt.Errorf("learned skill index recovered from backup: %w", primaryErr))
		}
	} else if !primaryLegacy {
		s.formatCurrent = true
	}
	for _, entry := range learned {
		if entry.Source == "learned" && validID(entry.ID) {
			s.index[entry.ID] = sanitizeLoadedEntry(entry)
		}
	}
	s.loadForgotten()
	if s.forgetCorrupt {
		for id, entry := range s.index {
			if entry.Source == "learned" {
				delete(s.index, id)
			}
		}
	}
	for id, marker := range s.forgotten {
		if entry, exists := s.index[id]; exists && entry.Generation != marker.Generation {
			delete(s.index, id)
		}
	}
	for _, entry := range s.index {
		if entry.LastOrder > s.nextOrder {
			s.nextOrder = entry.LastOrder
		}
	}
	_, markerErr := os.Stat(filepath.Join(s.root, "compaction-state.json"))
	ledgerExpected := markerErr == nil
	if err := s.loadCompactedTaskIDs(); err != nil && (!os.IsNotExist(err) || ledgerExpected) {
		s.ledgerCorrupt = true
		if os.IsNotExist(err) {
			err = errors.New("compacted task-id ledger is missing after compaction")
		}
		s.rememberError(err)
	}

	paths, _ := filepath.Glob(filepath.Join(s.root, "invocations", "*.json"))
	sort.Strings(paths)
	files := make([]receiptFile, 0, len(paths))
	retainedReceiptIDs := make(map[string]struct{}, len(paths))
	legacyEntries := make(map[string]Entry)
	for id, entry := range s.index {
		if entry.Source == "learned" && entry.Version < learnedEntryVersion {
			legacyEntries[id] = entry
			delete(s.index, id)
		}
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			s.rememberError(err)
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		file := receiptFile{path: path, size: info.Size()}
		files = append(files, file)
		s.receipts++
		s.receiptBytes += info.Size()
		raw, err := readBoundedFile(path, maxReceiptFileBytes)
		if err != nil {
			s.rememberError(fmt.Errorf("invalid MCP receipt %s: %w", filepath.Base(path), err))
			continue
		}
		var saved receipt
		if json.Unmarshal(raw, &saved) != nil {
			s.rememberError(fmt.Errorf("invalid MCP receipt: %s", filepath.Base(path)))
			continue
		}
		if saved.ID == "" {
			saved.ID = "legacy-" + digest(raw)
		}
		if !validReceipt(saved) {
			s.rememberError(fmt.Errorf("invalid MCP receipt: %s", filepath.Base(path)))
			continue
		}
		if saved.Version < learnedEntryVersion {
			saved.Arguments = scrubArguments(saved.Tool, saved.Action, saved.Arguments)
			saved.Outcome.Artifacts = sanitizeArtifactTypes(saved.Outcome.Artifacts, true)
		}
		if saved.Order > s.nextOrder {
			s.nextOrder = saved.Order
		}
		files[len(files)-1].data = saved
		files[len(files)-1].valid = true
		retainedReceiptIDs[saved.ID] = struct{}{}
	}
	sortReceiptFiles(files)
	for _, file := range files {
		if !file.valid {
			continue
		}
		saved := file.data
		if saved.TaskID != "" {
			s.taskIDs[saved.TaskID] = struct{}{}
			s.taskReceipts[saved.TaskID] = saved
		}
		if s.forgetCorrupt || s.indexCorrupt || s.formatCorrupt {
			continue
		}
		id := learnedID(saved.Tool, saved.Action)
		if marker, forgotten := s.forgotten[id]; forgotten && saved.Generation != marker.Generation {
			continue
		}
		entry := s.index[id]
		if entry.Version < learnedEntryVersion || !entryProcessedReceipt(entry, saved.ID) {
			entry = applyReceipt(entry, saved)
			s.index[id] = entry
		}
	}
	s.cleanProcessedReceiptsLocked(retainedReceiptIDs)
	// If a legacy index had no surviving receipt, retain its aggregate instead
	// of silently discarding history. It will migrate on the next completion.
	for id, legacy := range legacyEntries {
		if s.forgetCorrupt {
			break
		}
		if _, rebuilt := s.index[id]; !rebuilt {
			s.restoreLegacyEntry(legacy)
		}
	}
	if s.indexCorrupt || s.forgetCorrupt || s.formatCorrupt {
		return
	}
	if err := s.persistLearnedLocked(); err != nil {
		s.rememberError(err)
	}
	if err := s.repairMirrorsLocked(); err != nil {
		s.rememberError(err)
	}
	if err := s.pruneReceiptsLocked(files); err != nil {
		s.rememberError(err)
	}
}

func (s *Store) restoreLegacyEntry(entry Entry) {
	entry.Version = learnedEntryVersion
	entry.Triggers = []string{entry.Tool, entry.Action, entry.Tool + " " + entry.Action}
	entry.LastArguments = nil
	entry.LastKnownGoodArguments = nil
	entry.LastArtifacts = nil
	entry.LastKnownGoodArtifacts = nil
	entry.ProcessedReceipts = nil
	s.index[entry.ID] = sanitizeLoadedEntry(entry)
}

func (s *Store) loadStateFormat() {
	marker := filepath.Join(s.root, "skills", "state-format-v1")
	validMarker := false
	markerExists := false
	for _, path := range []string{marker, marker + ".bak"} {
		raw, err := readBoundedFile(path, 128)
		if err == nil {
			markerExists = true
			validMarker = validMarker || string(raw) == knowledgeStateMarker
		} else if !os.IsNotExist(err) {
			markerExists = true
		}
	}
	if validMarker {
		s.formatCurrent = true
	} else if markerExists {
		s.formatCurrent = true
		s.formatCorrupt = true
		s.indexCorrupt = true
		s.forgetCorrupt = true
		s.rememberError(errors.New("knowledge state format marker is untrusted"))
	}
	if !s.formatCurrent {
		for _, path := range []string{
			filepath.Join(s.root, "skills", "index.json"),
			filepath.Join(s.root, "skills", "index.json.bak"),
			filepath.Join(s.root, "skills", "forgotten.json"),
			filepath.Join(s.root, "skills", "forgotten.json.bak"),
		} {
			if looksLikeCurrentKnowledgeEnvelope(path) {
				s.formatCurrent = true
				break
			}
		}
	}
}

func looksLikeCurrentKnowledgeEnvelope(path string) bool {
	raw, err := readBoundedFile(path, maxKnowledgeIndexBytes)
	if err != nil || len(raw) == 0 || raw[0] != '{' {
		return false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return false
	}
	_, version := object["version"]
	_, checksum := object["checksum"]
	_, entries := object["entries"]
	_, markers := object["markers"]
	return version && checksum && (entries || markers)
}

func (s *Store) loadForgotten() {
	path := filepath.Join(s.root, "skills", "forgotten.json")
	primary, primaryLegacy, primaryErr := readForgotten(path)
	if primaryErr == nil && primaryLegacy && s.formatCurrent {
		primaryErr = errors.New("forget state format downgrade refused")
	}
	backup, backupLegacy, backupErr := readForgotten(path + ".bak")
	if backupErr == nil && backupLegacy && s.formatCurrent {
		backupErr = errors.New("forget state backup format downgrade refused")
	}
	if primaryErr != nil && backupErr != nil {
		if s.formatCurrent || !os.IsNotExist(primaryErr) || !os.IsNotExist(backupErr) {
			s.forgetCorrupt = true
			s.rememberError(fmt.Errorf("forget state is untrusted: primary=%v; backup=%v", primaryErr, backupErr))
		}
		return
	}
	markers := primary
	legacy := primaryLegacy
	if primaryErr != nil {
		markers, legacy = backup, backupLegacy
		s.rememberError(fmt.Errorf("forget state recovered from backup: %w", primaryErr))
	} else if backupErr == nil {
		// Backup-first atomic writes can leave a newer valid backup beside a
		// stale valid primary after power loss. Tombstone generations are
		// monotonic, so merge by maximum generation instead of trusting the
		// primary filename and accidentally resurrecting a forgotten skill.
		markers = mergeForgetMarkers(primary, backup)
	}
	if !legacy {
		s.formatCurrent = true
	}
	for id, marker := range markers {
		if validID(id) && marker.Generation > 0 {
			s.forgotten[id] = marker
		}
	}
}

func mergeForgetMarkers(primary, backup map[string]forgetMarker) map[string]forgetMarker {
	merged := make(map[string]forgetMarker, len(primary)+len(backup))
	for id, marker := range backup {
		merged[id] = marker
	}
	for id, marker := range primary {
		if existing, ok := merged[id]; !ok || marker.Generation >= existing.Generation {
			merged[id] = marker
		}
	}
	return merged
}

func readForgotten(path string) (map[string]forgetMarker, bool, error) {
	raw, err := readBoundedFile(path, maxForgetStateBytes)
	if err != nil {
		return nil, false, err
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return nil, false, errors.New("forget state JSON is invalid")
	}
	if _, hasVersion := object["version"]; hasVersion || object["markers"] != nil || object["checksum"] != nil {
		var envelope forgetState
		if json.Unmarshal(raw, &envelope) != nil || envelope.Version != forgetStateVersion || !verifyForgetState(envelope) {
			return nil, false, errors.New("forget state checksum mismatch")
		}
		return envelope.Markers, false, nil
	}
	var current map[string]forgetMarker
	if json.Unmarshal(raw, &current) == nil {
		return current, true, nil
	}
	// Version 3 stored wall-clock cutoffs. Migrate them once to generation 1.
	var legacy map[string]time.Time
	if json.Unmarshal(raw, &legacy) == nil {
		migrated := make(map[string]forgetMarker, len(legacy))
		for id, at := range legacy {
			migrated[id] = forgetMarker{Generation: 1, At: at}
		}
		return migrated, true, nil
	}
	return nil, false, errors.New("forget state is invalid")
}

func (s *Store) Match(query string, limit int) []Match {
	if limit <= 0 || limit > 3 {
		limit = 3
	}
	query = strings.ToLower(strings.TrimSpace(query))
	terms := splitWords(query)
	termSet := make(map[string]struct{}, len(terms))
	for _, term := range terms {
		termSet[term] = struct{}{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	matches := make([]Match, 0, len(s.index))
	for _, entry := range s.index {
		score := 0
		for _, trigger := range entry.Triggers {
			trigger = strings.ToLower(strings.TrimSpace(trigger))
			if trigger == "" {
				continue
			}
			if query == trigger {
				score += 12
			} else if _, exists := termSet[trigger]; exists {
				score += 7
			} else if strings.HasPrefix(trigger, "aplm_") && strings.Contains(query, trigger) {
				score += 10
			}
		}
		keywords := make(map[string]struct{}, len(entry.Keywords)+5)
		for _, value := range append([]string{entry.ID, entry.Title, entry.Tool, entry.Action}, entry.Keywords...) {
			for _, word := range splitWords(value) {
				keywords[word] = struct{}{}
			}
		}
		for _, term := range terms {
			if _, exists := keywords[term]; exists {
				score++
				if term == strings.ToLower(entry.Tool) || term == strings.ToLower(entry.Action) {
					score += 3
				}
			}
		}
		if score > 0 {
			matches = append(matches, Match{ID: entry.ID, Title: entry.Title, Summary: entry.Summary, Source: entry.Source, Score: score})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Score != matches[j].Score {
			return matches[i].Score > matches[j].Score
		}
		return matches[i].ID < matches[j].ID
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches
}

func (s *Store) List(source string) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]Entry, 0, len(s.index))
	for _, entry := range s.index {
		if source == "" || source == "all" || entry.Source == source {
			copy := entry
			copy.Keywords = nil
			copy.Triggers = nil
			copy.LastArguments = nil
			copy.LastKnownGoodArguments = nil
			copy.ProcessedReceipts = nil
			items = append(items, copy)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

func (s *Store) ListIndex(source string, offset, limit int) ([]IndexItem, int) {
	if offset < 0 {
		offset = 0
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	s.mu.RLock()
	items := make([]IndexItem, 0, len(s.index))
	for _, entry := range s.index {
		if source == "" || source == "all" || entry.Source == source {
			items = append(items, IndexItem{ID: entry.ID, Title: entry.Title, Summary: entry.Summary, Source: entry.Source, UpdatedAt: entry.UpdatedAt})
		}
	}
	s.mu.RUnlock()
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	total := len(items)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return items[offset:end], total
}

func (s *Store) Get(id string) (Entry, string, error) {
	id = strings.TrimSpace(id)
	s.mu.RLock()
	entry, ok := s.index[id]
	if !ok {
		s.mu.RUnlock()
		return Entry{}, "", os.ErrNotExist
	}
	entry.Keywords = nil
	entry.Triggers = nil
	entry.LastArguments = nil
	entry.LastKnownGoodArguments = nil
	entry.ProcessedReceipts = nil
	if body, exists := s.builtins[id]; exists {
		s.mu.RUnlock()
		if len(body) > maxSkillBodyBytes {
			return Entry{}, "", errors.New("技能正文超过 64 KiB 硬上限")
		}
		return entry, body, nil
	}
	s.mu.RUnlock()
	raw, err := readBoundedFile(filepath.Join(s.root, "skills", filepath.FromSlash(id), "SKILL.md"), maxSkillBodyBytes)
	if err != nil {
		return Entry{}, "", err
	}
	return entry, string(raw), nil
}

func (s *Store) Stats() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	builtIn, learned, attempts := 0, 0, 0
	for _, entry := range s.index {
		if entry.Source == "builtin" {
			builtIn++
		} else {
			learned++
			attempts += entry.Attempts
		}
	}
	return map[string]any{
		"builtIn": builtIn, "learned": learned, "attempts": attempts,
		"receipts": s.receipts, "retainedReceipts": s.receipts, "receiptBytes": s.receiptBytes,
		"receiptLimits": map[string]any{"files": maxReceiptFiles, "bytes": maxReceiptBytes, "compaction": "aggregate skills + checksummed exact task-id digest ledger"},
		"root":          s.root, "mirror": s.mirror, "completionLedgerTrusted": !s.ledgerCorrupt, "forgetStateTrusted": !s.forgetCorrupt,
		"skillIndexTrusted": !s.indexCorrupt, "knowledgeStateFormatTrusted": !s.formatCorrupt, "lastPersistenceError": s.lastError,
	}
}

// CompletionResult returns independent terminal-journal evidence for a durable
// task. It is used before restart recovery so a stale running snapshot can
// never cause an already-completed destructive operation to execute twice.
func (s *Store) CompletionResult(taskID string) (model.Result, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if saved, exists := s.taskReceipts[taskID]; exists {
		var result model.Result
		if saved.Outcome.Success {
			result = model.Success(saved.Outcome.Code, "已从 MCP 完成日志恢复终态", map[string]any{"receiptId": saved.ID, "artifactTypes": saved.Outcome.Artifacts})
		} else {
			result = model.Failure(saved.Outcome.Code, "已从 MCP 完成日志恢复失败终态", empty(saved.Outcome.ErrorClass, "RecoveredFailure"), "原始敏感错误详情未写入长期记忆")
		}
		result.TaskID = taskID
		result.Strategy = saved.Outcome.Strategy
		result.DurationMS = saved.Outcome.DurationMS
		result.ExitCode = saved.Outcome.ExitCode
		result.RebootRequired = saved.Outcome.RebootRequired
		return result, true
	}
	if _, exists := s.compactedIDs[taskDigest(taskID)]; exists {
		result := model.Failure("COMPLETION_COMPACTED", "任务已完成且回执已压缩；为防止重复执行，不自动重试", "RecoveredCompletion", "详细终态已按保留策略压缩到聚合技能")
		result.TaskID = taskID
		result.Strategy = "exact_completion_ledger"
		return result, true
	}
	if s.ledgerCorrupt {
		result := model.Failure("COMPLETION_LEDGER_UNTRUSTED", "任务完成账本校验失败；为防止重复执行，未自动重试", "RecoveredCompletion", "请检查 aplm_skill.stats.lastPersistenceError，读回目标状态后再显式提交新任务")
		result.TaskID = taskID
		result.Strategy = "completion_ledger_fail_closed"
		return result, true
	}
	return model.Result{}, false
}

// RetainCompletionTasks bounds restart-deduplication state to task snapshots
// that can still be recovered. Invocation receipts remain available for skill
// learning until the independent receipt retention policy compacts them.
func (s *Store) RetainCompletionTasks(ids []string) error {
	allowedIDs := make(map[string]struct{}, len(ids))
	allowedDigests := make(map[[32]byte]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		allowedIDs[id] = struct{}{}
		allowedDigests[taskDigest(id)] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ledgerCorrupt {
		return errors.New("completion ledger is untrusted; retention update refused")
	}
	changed := false
	for digest := range s.compactedIDs {
		if _, keep := allowedDigests[digest]; !keep {
			delete(s.compactedIDs, digest)
			changed = true
		}
	}
	for id := range s.taskReceipts {
		if _, keep := allowedIDs[id]; !keep {
			delete(s.taskReceipts, id)
			delete(s.taskIDs, id)
		}
	}
	_, ledgerErr := os.Stat(filepath.Join(s.root, "compacted-task-ids.bin"))
	if !changed && os.IsNotExist(ledgerErr) {
		return nil
	}
	if err := s.writeCompactedTaskIDsLocked(); err != nil {
		s.lastError = err.Error()
		return err
	}
	return nil
}

func (s *Store) Record(tool string, args map[string]any, result model.Result) error {
	action, _ := args["action"].(string)
	tool = strings.ToLower(strings.TrimSpace(tool))
	action = strings.ToLower(strings.TrimSpace(action))
	if !safeIdentifier.MatchString(tool) || !safeIdentifier.MatchString(action) {
		return errors.New("tool/action is not a safe learned-skill identifier")
	}
	safeArgs := scrubArguments(tool, action, args)
	canonical, _ := json.Marshal(map[string]any{"tool": tool, "action": action, "arguments": safeArgs})
	fingerprint := digest(canonical)
	now := time.Now().UTC()
	receiptID, err := randomHex(16)
	if err != nil {
		return err
	}
	r := receipt{
		Version: learnedEntryVersion, ID: receiptID, CompletedAt: now, TaskID: result.TaskID, Tool: tool, Action: action, Fingerprint: fingerprint, Arguments: safeArgs,
		Outcome: outcome{Success: result.Success, Code: truncateKnowledgeText(result.Code, 128), Strategy: truncateKnowledgeText(result.Strategy, 128), DurationMS: result.DurationMS, ExitCode: result.ExitCode, RebootRequired: result.RebootRequired},
	}
	if result.Error != nil {
		r.Outcome.ErrorClass = truncateKnowledgeText(result.Error.Type, 128)
		r.Outcome.ErrorDigest = digest([]byte(result.Error.Type + ":" + result.Code))
	}
	for _, artifact := range result.Artifacts {
		if len(r.Outcome.Artifacts) >= maxLearnedArtifactTypes {
			break
		}
		name := filepath.Base(artifact.Path)
		if name == "." || name == "" {
			name = artifact.Name
		}
		if name != "" {
			extension := safeArtifactType(filepath.Ext(name))
			r.Outcome.Artifacts = appendUniqueSorted(r.Outcome.Artifacts, extension)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.indexCorrupt {
		return errors.New("learned skill index is untrusted; updates are disabled")
	}
	if s.forgetCorrupt {
		return errors.New("forget state is untrusted; learned-skill updates are disabled")
	}
	id := learnedID(tool, action)
	r.Generation = s.forgotten[id].Generation
	if s.nextOrder >= ^uint64(0)-1 {
		return errors.New("completion receipt order is exhausted")
	}
	s.nextOrder++
	r.Order = s.nextOrder
	raw, _ := json.MarshalIndent(r, "", "  ")
	raw = append(raw, '\n')
	if len(raw) > maxReceiptFileBytes {
		return fmt.Errorf("completion receipt exceeds %d bytes", maxReceiptFileBytes)
	}
	receiptName := now.Format("20060102T150405.000000000Z") + "-" + fingerprint[:12] + "-" + receiptID + ".json"
	if result.TaskID != "" && s.taskAlreadyRecorded(result.TaskID) {
		return s.repairLocked()
	}
	if err := atomicfile.Write(filepath.Join(s.root, "invocations", receiptName), raw, 0o600); err != nil {
		s.lastError = err.Error()
		return err
	}
	s.receipts++
	s.receiptBytes += int64(len(raw))
	if result.TaskID != "" {
		s.taskIDs[result.TaskID] = struct{}{}
		s.taskReceipts[result.TaskID] = r
	}
	s.index[id] = applyReceipt(s.index[id], r)
	if err := s.persistLearnedLocked(); err != nil {
		s.lastError = err.Error()
		return err
	}
	if err := s.repairMirrorsLocked(); err != nil {
		s.lastError = err.Error()
		return nil
	}
	if err := s.pruneReceiptsLocked(nil); err != nil {
		s.lastError = err.Error()
		return nil
	}
	s.lastError = ""
	return nil
}

// CompletionArguments returns the bounded structural envelope retained only
// when a terminal completion receipt could not be committed. It is safe to
// persist and may be passed back to Record on retry.
func CompletionArguments(tool string, args map[string]any) map[string]any {
	action, _ := args["action"].(string)
	return scrubArguments(strings.ToLower(strings.TrimSpace(tool)), strings.ToLower(strings.TrimSpace(action)), args)
}

func applyReceipt(entry Entry, latest receipt) Entry {
	id := learnedID(latest.Tool, latest.Action)
	if entry.ID == "" || entry.Version < learnedEntryVersion || entry.Generation != latest.Generation {
		entry = Entry{Version: learnedEntryVersion, ID: id, Source: "learned", Tool: latest.Tool, Action: latest.Action, Generation: latest.Generation, FailureClasses: make(map[string]int)}
	}
	isLatest := false
	if latest.Order > 0 {
		isLatest = latest.Order > entry.LastOrder
	} else if entry.LastOrder == 0 {
		isLatest = entry.UpdatedAt.IsZero() || latest.CompletedAt.After(entry.UpdatedAt)
	}
	hadUnrecoveredFailure := false
	if latest.Order > 0 {
		hadUnrecoveredFailure = isLatest && entry.LastFailureOrder > entry.LastSuccessOrder
	} else {
		hadUnrecoveredFailure = isLatest && entry.LastFailureAt.After(entry.LastSuccessAt)
	}
	entry.Version = learnedEntryVersion
	entry.ID = id
	entry.Title = fmt.Sprintf("%s / %s 执行经验", latest.Tool, latest.Action)
	entry.Source = "learned"
	entry.Tool = latest.Tool
	entry.Action = latest.Action
	entry.Generation = latest.Generation
	entry.Attempts++
	if isLatest {
		entry.LastOrder = latest.Order
		entry.UpdatedAt = latest.CompletedAt
		entry.LastCode = latest.Outcome.Code
		entry.LastStrategy = latest.Outcome.Strategy
		entry.LastErrorClass = latest.Outcome.ErrorClass
		entry.LastDurationMS = latest.Outcome.DurationMS
		entry.LastExitCode = latest.Outcome.ExitCode
		entry.LastFingerprint = latest.Fingerprint
		entry.LastArguments = cloneJSONMap(latest.Arguments)
		entry.LastRebootRequired = latest.Outcome.RebootRequired
		entry.LastArtifacts = append([]string(nil), latest.Outcome.Artifacts...)
	}
	if !entryProcessedReceipt(entry, latest.ID) {
		entry.ProcessedReceipts = append(entry.ProcessedReceipts, latest.ID)
	}
	if latest.Outcome.Success {
		entry.Successes++
		if hadUnrecoveredFailure {
			entry.Recoveries++
		}
		successIsLatest := latest.Order > entry.LastSuccessOrder || latest.Order == 0 && entry.LastSuccessOrder == 0 && (entry.LastSuccessAt.IsZero() || latest.CompletedAt.After(entry.LastSuccessAt))
		if successIsLatest {
			entry.LastSuccessAt = latest.CompletedAt
			entry.LastSuccessOrder = latest.Order
			entry.LastKnownGoodAt = latest.CompletedAt
			entry.LastKnownGoodCode = latest.Outcome.Code
			entry.LastKnownGoodStrategy = latest.Outcome.Strategy
			entry.LastKnownGoodArguments = cloneJSONMap(latest.Arguments)
			entry.LastKnownGoodReboot = latest.Outcome.RebootRequired
			entry.LastKnownGoodArtifacts = append([]string(nil), latest.Outcome.Artifacts...)
		}
	} else {
		failureIsLatest := latest.Order > entry.LastFailureOrder || latest.Order == 0 && entry.LastFailureOrder == 0 && (entry.LastFailureAt.IsZero() || latest.CompletedAt.After(entry.LastFailureAt))
		if failureIsLatest {
			entry.LastFailureAt = latest.CompletedAt
			entry.LastFailureOrder = latest.Order
		}
		if entry.FailureClasses == nil {
			entry.FailureClasses = make(map[string]int)
		}
		class := empty(latest.Outcome.ErrorClass, "Unclassified")
		if _, exists := entry.FailureClasses[class]; exists || len(entry.FailureClasses) < 32 {
			entry.FailureClasses[class]++
		} else {
			entry.FailureClasses["Other"]++
		}
	}
	entry.Summary = fmt.Sprintf("基于 %d 次真实完成记录：成功 %d 次，已观测失败后恢复 %d 次；最近结果 %s。", entry.Attempts, entry.Successes, entry.Recoveries, entry.LastCode)
	entry.Triggers = []string{latest.Tool, latest.Action, latest.Tool + " " + latest.Action}
	entry.Keywords = splitWords(latest.Tool + " " + latest.Action + " " + latest.Outcome.Code + " " + latest.Outcome.Strategy + " " + latest.Outcome.ErrorClass)
	return sanitizeLoadedEntry(entry)
}

func (s *Store) Forget(id string) error {
	if !strings.HasPrefix(id, "learned/") || !validID(id) {
		return errors.New("只能删除 learned/ 下的自动技能")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.indexCorrupt {
		return errors.New("learned skill index is untrusted; forgetting is disabled")
	}
	if s.forgetCorrupt {
		return errors.New("forget state is untrusted; refusing to replace tombstones")
	}
	_, exists := s.index[id]
	oldMarker, pending := s.forgotten[id]
	if !exists && !pending {
		return os.ErrNotExist
	}
	entryGeneration := uint64(0)
	if entry, ok := s.index[id]; ok {
		entryGeneration = entry.Generation
	}
	nextGeneration := oldMarker.Generation
	if entryGeneration > nextGeneration {
		nextGeneration = entryGeneration
	}
	if nextGeneration >= ^uint64(0)-1 {
		return errors.New("forget generation is exhausted")
	}
	nextGeneration++
	s.forgotten[id] = forgetMarker{Generation: nextGeneration, At: time.Now().UTC()}
	// Persist the generation cutoff first. If any later cleanup step fails,
	// restart recovery still cannot resurrect pre-forget receipts.
	if err := s.writeForgottenLocked(); err != nil {
		if pending {
			s.forgotten[id] = oldMarker
		} else {
			delete(s.forgotten, id)
		}
		return err
	}
	delete(s.index, id)
	if err := s.writeIndexLocked(); err != nil {
		return err
	}
	if err := s.removeLearnedFilesLocked(id); err != nil {
		s.lastError = err.Error()
		return err
	}
	s.lastError = ""
	return nil
}

func (s *Store) repairLocked() error {
	if err := s.persistLearnedLocked(); err != nil {
		s.lastError = err.Error()
		return err
	}
	if err := s.repairMirrorsLocked(); err != nil {
		s.lastError = err.Error()
		return nil
	}
	s.lastError = ""
	return nil
}

func (s *Store) persistLearnedLocked() error {
	if s.indexCorrupt || s.forgetCorrupt || s.formatCorrupt {
		return errors.New("knowledge state is untrusted; refusing persistent repair")
	}
	if err := s.cleanupForgottenLocked(); err != nil {
		return err
	}
	for _, entry := range s.index {
		if entry.Source != "learned" {
			continue
		}
		path := filepath.Join(s.root, "skills", filepath.FromSlash(entry.ID), "SKILL.md")
		if err := atomicfile.Write(path, []byte(renderLearned(entry)), 0o600); err != nil {
			return err
		}
	}
	if err := s.writeIndexLocked(); err != nil {
		return err
	}
	if err := s.writeForgottenLocked(); err != nil {
		return err
	}
	return s.writeStateMarkerLocked()
}

func (s *Store) cleanupForgottenLocked() error {
	var first error
	for id, marker := range s.forgotten {
		if entry, exists := s.index[id]; exists && entry.Generation == marker.Generation {
			continue
		}
		if err := s.removeLearnedFilesLocked(id); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *Store) removeLearnedFilesLocked(id string) error {
	var first error
	for _, root := range []string{filepath.Join(s.root, "skills"), s.mirror} {
		path := filepath.Clean(filepath.Join(root, filepath.FromSlash(id)))
		if filepath.Dir(path) != filepath.Clean(filepath.Join(root, "learned")) {
			return errors.New("技能路径越界")
		}
		if err := os.Remove(filepath.Join(path, "SKILL.md")); err != nil && !os.IsNotExist(err) && first == nil {
			first = err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) && first == nil {
			first = err
		}
	}
	return first
}

func (s *Store) repairMirrorsLocked() error {
	var first error
	for _, entry := range s.index {
		if entry.Source != "learned" {
			continue
		}
		path := filepath.Join(s.mirror, filepath.FromSlash(entry.ID), "SKILL.md")
		if err := atomicfile.Write(path, []byte(renderLearned(entry)), 0o600); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *Store) writeIndexLocked() error {
	if s.indexCorrupt || s.formatCorrupt {
		return errors.New("refusing to overwrite an untrusted learned skill index")
	}
	items := make([]Entry, 0)
	for _, entry := range s.index {
		if entry.Source == "learned" {
			items = append(items, entry)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	canonical, err := json.Marshal(items)
	if err != nil {
		return err
	}
	state := indexState{Version: indexStateVersion, Entries: items, Checksum: digest(canonical)}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if len(raw) > maxKnowledgeIndexBytes {
		return errors.New("learned skill index exceeds the 8 MiB limit")
	}
	path := filepath.Join(s.root, "skills", "index.json")
	if err := atomicfile.Write(path+".bak", raw, 0o600); err != nil {
		return err
	}
	return atomicfile.Write(path, raw, 0o600)
}

func readKnowledgeIndex(path string) ([]Entry, bool, error) {
	raw, err := readBoundedFile(path, maxKnowledgeIndexBytes)
	if err != nil {
		return nil, false, err
	}
	var state indexState
	if json.Unmarshal(raw, &state) == nil && state.Version == indexStateVersion {
		canonical, err := json.Marshal(state.Entries)
		if err != nil {
			return nil, false, err
		}
		if state.Checksum == "" || !strings.EqualFold(state.Checksum, digest(canonical)) {
			return nil, false, errors.New("learned skill index checksum mismatch")
		}
		return state.Entries, false, nil
	}
	// Versions before the checksummed envelope stored the learned entry slice
	// directly. Accept it once and migrate both primary and backup on write.
	var legacy []Entry
	if json.Unmarshal(raw, &legacy) != nil {
		return nil, false, errors.New("learned skill index JSON is invalid")
	}
	return legacy, true, nil
}

func (s *Store) writeForgottenLocked() error {
	if s.forgetCorrupt || s.formatCorrupt {
		return errors.New("forget state is untrusted; refusing to overwrite tombstones")
	}
	state := forgetState{Version: forgetStateVersion, Markers: s.forgotten}
	canonical, err := json.Marshal(state.Markers)
	if err != nil {
		return err
	}
	state.Checksum = digest(canonical)
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	path := filepath.Join(s.root, "skills", "forgotten.json")
	if err := atomicfile.Write(path+".bak", raw, 0o600); err != nil {
		return err
	}
	return atomicfile.Write(path, raw, 0o600)
}

func (s *Store) writeStateMarkerLocked() error {
	if s.formatCorrupt {
		return errors.New("knowledge state format marker is untrusted")
	}
	path := filepath.Join(s.root, "skills", "state-format-v1")
	for _, target := range []string{path + ".bak", path} {
		if err := atomicfile.Write(target, []byte(knowledgeStateMarker), 0o600); err != nil {
			return err
		}
	}
	s.formatCurrent = true
	return nil
}

func renderLearned(entry Entry) string {
	status := "失败"
	if entry.LastOrder > 0 && entry.LastSuccessOrder == entry.LastOrder || entry.LastOrder == 0 && entry.LastSuccessAt.Equal(entry.UpdatedAt) {
		status = "成功"
	}
	lastTemplate := safeJSON(entry.LastArguments)
	goodTemplate := "暂无已验证成功模板"
	if entry.LastKnownGoodArguments != nil {
		goodTemplate = safeJSON(entry.LastKnownGoodArguments)
		if goodTemplate == lastTemplate {
			goodTemplate = "同最近调用（该次即为最近已知成功）"
		}
	}
	failures := failureSummary(entry.FailureClasses)
	recovery := "尚未观测到“失败后再成功”的闭环。"
	if entry.Recoveries > 0 {
		recovery = fmt.Sprintf("已观测 %d 次失败后恢复；最近失败=%s，最近已知可用=%s。时序只证明恢复，不自动推断因果。", entry.Recoveries, formatTime(entry.LastFailureAt), formatTime(entry.LastKnownGoodAt))
	}
	return fmt.Sprintf(`# %s

## 自动触发

- 工具：%s
- 动作：%s
- 触发时机：准备调用同一工具/动作时先加载本文；其他场景仅使用摘要，不把全文塞入上下文。

## 经验统计

- 真实完成：%d 次；成功：%d 次；失败后恢复：%d 次。
- 最近一次：%s，code=%s，strategy=%s，duration=%dms，exitCode=%d，errorClass=%s，rebootRequired=%t，产物类型=%s。
- 历史失败分类：%s。
- 恢复链：%s

## 脱敏可复用模板

最近调用：

~~~json
%s
~~~

最近已知可用（code=%s，strategy=%s，rebootRequired=%t，产物类型=%s）：

~~~json
%s
~~~

## 下次执行清单

1. 先核对 action 与 Schema，只传当前任务需要的字段；不盲目复制旧路径、包名、用户 ID 或网络端点。
2. 破坏性操作先读回目标当前状态并确认精确范围；有安全上限时使用分页、长度、文件数或超时参数。
3. 优先从“最近已知可用”模板开始，但先重新探测会漂移的设备/文件/网络状态。
4. 执行后同时检查 success、code、strategy 与语义读回；不把“进程退出 0”单独当成业务成功。
5. 如失败，先按 errorClass/code 归类，保留证据并只修正已验证的根因；不根据一次时序相邻臆造“修复方法”。
6. 大输出读 artifact/分页结果；摘要已足够时不加载本全文。

## 隐私与证据边界

本技能由 MCP 真实完成回执汇总。密钥、Cookie、命令/脚本/消息/正文、查询值和大字符串只保存类型与长度，URL 只保存结构。它只证明工具级历史，不代表当前环境必然一致。最新回执指纹：%s。
`, entry.Title, entry.Tool, entry.Action, entry.Attempts, entry.Successes, entry.Recoveries, status, entry.LastCode, entry.LastStrategy, entry.LastDurationMS, entry.LastExitCode, empty(entry.LastErrorClass, "none"), entry.LastRebootRequired, empty(strings.Join(entry.LastArtifacts, ","), "none"), failures, recovery, lastTemplate, empty(entry.LastKnownGoodCode, "none"), empty(entry.LastKnownGoodStrategy, "none"), entry.LastKnownGoodReboot, empty(strings.Join(entry.LastKnownGoodArtifacts, ","), "none"), goodTemplate, entry.LastFingerprint)
}

func failureSummary(classes map[string]int) string {
	if len(classes) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(classes))
	for key := range classes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, classes[key]))
	}
	return strings.Join(parts, ", ")
}

func safeJSON(value any) string {
	if value == nil {
		return "{}"
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "none"
	}
	return value.UTC().Format(time.RFC3339)
}

func (s *Store) pruneReceiptsLocked(known []receiptFile) error {
	if s.receipts <= maxReceiptFiles && s.receiptBytes <= maxReceiptBytes {
		return nil
	}
	files := known
	if files == nil {
		paths, _ := filepath.Glob(filepath.Join(s.root, "invocations", "*.json"))
		sort.Strings(paths)
		files = make([]receiptFile, 0, len(paths))
		for _, path := range paths {
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			file := receiptFile{path: path, size: info.Size()}
			raw, err := readBoundedFile(path, maxReceiptFileBytes)
			if err != nil {
				files = append(files, file)
				continue
			}
			var saved receipt
			if json.Unmarshal(raw, &saved) == nil && validReceipt(saved) {
				file.data = saved
				file.valid = true
			}
			files = append(files, file)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	remainingCount := s.receipts
	remainingBytes := s.receiptBytes
	remove := make([]receiptFile, 0)
	for _, file := range files {
		if remainingCount <= receiptLowWaterFiles && remainingBytes <= receiptLowWaterBytes {
			break
		}
		remove = append(remove, file)
		remainingCount--
		remainingBytes -= file.size
		if file.valid && file.data.TaskID != "" {
			s.compactedIDs[taskDigest(file.data.TaskID)] = struct{}{}
		}
	}
	if len(remove) == 0 {
		return errors.New("receipt retention limits exceeded but no valid receipts can be compacted")
	}
	if err := s.writeCompactedTaskIDsLocked(); err != nil {
		return err
	}
	removedReceiptIDs := make(map[string]struct{}, len(remove))
	for _, file := range remove {
		if err := os.Remove(file.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		delete(s.taskIDs, file.data.TaskID)
		delete(s.taskReceipts, file.data.TaskID)
		removedReceiptIDs[file.data.ID] = struct{}{}
		s.receipts--
		s.receiptBytes -= file.size
	}
	for id, entry := range s.index {
		if entry.Source != "learned" {
			continue
		}
		kept := entry.ProcessedReceipts[:0]
		for _, receiptID := range entry.ProcessedReceipts {
			if _, removed := removedReceiptIDs[receiptID]; !removed {
				kept = append(kept, receiptID)
			}
		}
		entry.ProcessedReceipts = kept
		s.index[id] = entry
	}
	return s.writeIndexLocked()
}

func (s *Store) taskAlreadyRecorded(id string) bool {
	if _, exists := s.taskIDs[id]; exists {
		return true
	}
	_, exists := s.compactedIDs[taskDigest(id)]
	return exists
}

func taskDigest(id string) [32]byte {
	return sha256.Sum256([]byte(id))
}

func (s *Store) loadCompactedTaskIDs() error {
	raw, err := readBoundedFile(filepath.Join(s.root, "compacted-task-ids.bin"), maxReceiptBytes)
	if err != nil {
		return err
	}
	if len(raw) < 16+sha256.Size || string(raw[:8]) != compactedTaskMagic {
		return errors.New("invalid compacted task-id ledger header")
	}
	count := binary.BigEndian.Uint64(raw[8:16])
	if count > uint64((len(raw)-16-sha256.Size)/sha256.Size) || len(raw) != 16+int(count)*sha256.Size+sha256.Size {
		return errors.New("invalid compacted task-id ledger length")
	}
	payload := raw[:len(raw)-sha256.Size]
	want := sha256.Sum256(payload)
	if !bytes.Equal(want[:], raw[len(raw)-sha256.Size:]) {
		return errors.New("compacted task-id ledger checksum mismatch")
	}
	for index := uint64(0); index < count; index++ {
		start := 16 + int(index)*sha256.Size
		var digest [32]byte
		copy(digest[:], raw[start:start+sha256.Size])
		s.compactedIDs[digest] = struct{}{}
	}
	return nil
}

func (s *Store) writeCompactedTaskIDsLocked() error {
	if s.ledgerCorrupt {
		return errors.New("refusing to overwrite an untrusted compacted task-id ledger")
	}
	digests := make([][32]byte, 0, len(s.compactedIDs))
	for digest := range s.compactedIDs {
		digests = append(digests, digest)
	}
	sort.Slice(digests, func(i, j int) bool { return bytes.Compare(digests[i][:], digests[j][:]) < 0 })
	raw := make([]byte, 16, 16+len(digests)*sha256.Size+sha256.Size)
	copy(raw[:8], compactedTaskMagic)
	binary.BigEndian.PutUint64(raw[8:16], uint64(len(digests)))
	for _, digest := range digests {
		raw = append(raw, digest[:]...)
	}
	checksum := sha256.Sum256(raw)
	raw = append(raw, checksum[:]...)
	if err := atomicfile.Write(filepath.Join(s.root, "compacted-task-ids.bin"), raw, 0o600); err != nil {
		return err
	}
	marker, _ := json.Marshal(map[string]any{"version": 1, "compacted": true})
	marker = append(marker, '\n')
	return atomicfile.Write(filepath.Join(s.root, "compaction-state.json"), marker, 0o600)
}

func validReceipt(saved receipt) bool {
	return saved.ID != "" && len(saved.ID) <= 128 && len(saved.TaskID) <= 128 &&
		!saved.CompletedAt.IsZero() && safeIdentifier.MatchString(saved.Tool) && safeIdentifier.MatchString(saved.Action) &&
		len(saved.Fingerprint) == sha256.Size*2 && saved.Order != ^uint64(0) && saved.Generation != ^uint64(0)
}

func entryProcessedReceipt(entry Entry, receiptID string) bool {
	for _, existing := range entry.ProcessedReceipts {
		if existing == receiptID {
			return true
		}
	}
	return false
}

func (s *Store) cleanProcessedReceiptsLocked(retained map[string]struct{}) {
	for id, entry := range s.index {
		if entry.Source != "learned" {
			continue
		}
		kept := entry.ProcessedReceipts[:0]
		for _, receiptID := range entry.ProcessedReceipts {
			if _, exists := retained[receiptID]; exists {
				kept = append(kept, receiptID)
			}
		}
		entry.ProcessedReceipts = kept
		s.index[id] = entry
	}
}

func learnedID(tool, action string) string {
	return "learned/" + slug(tool+"-"+action)
}

func randomHex(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func (s *Store) rememberError(err error) {
	if err != nil && s.lastError == "" {
		s.lastError = err.Error()
	}
}

func cloneJSONMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	raw, _ := json.Marshal(source)
	var clone map[string]any
	_ = json.Unmarshal(raw, &clone)
	return clone
}

const (
	maxTemplateDepth = 6
	maxTemplateNodes = 256
	maxTemplateBytes = 8 << 10
)

var reusableStringKeys = map[string]struct{}{
	"action": {}, "algorithm": {}, "encoding": {}, "format": {}, "identity": {}, "mode": {}, "namespace": {},
	"user": {}, "type": {}, "method": {}, "table": {}, "role": {}, "stream": {}, "locale": {}, "timezone": {},
	"extension": {}, "ext": {}, "operation": {}, "service": {}, "kind": {}, "context": {}, "lower": {},
	"expected": {}, "digest": {}, "hash": {}, "sha256": {},
}

var pathTemplateKeys = map[string]struct{}{
	"path": {}, "source": {}, "destination": {}, "output": {}, "cwd": {}, "root": {}, "target": {}, "lower": {}, "upper": {}, "work": {}, "src": {}, "dst": {},
}

var pathArrayTemplateKeys = map[string]struct{}{
	"sources": {}, "sourcepaths": {}, "paths": {}, "apks": {}, "inputs": {},
}

type scrubBudget struct {
	nodes int
}

func scrubArguments(tool, action string, source map[string]any) map[string]any {
	budget := &scrubBudget{}
	value := scrubObject(source, 0, budget)
	value["action"] = action
	raw, _ := json.Marshal(value)
	if len(raw) <= maxTemplateBytes {
		return value
	}
	// Fall back to a bounded field-shape summary. This keeps every receipt small
	// even when batch/targetArgs objects contain thousands of short values.
	keys := make([]string, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > 64 {
		keys = keys[:64]
	}
	compact := map[string]any{"action": action, "_templateTruncated": true, "_tool": tool, "_fieldCount": len(source)}
	for _, key := range keys {
		if strings.EqualFold(key, "action") {
			continue
		}
		compact[key] = redactedDescriptor(source[key], "template_bytes")
	}
	return compact
}

func scrubObject(source map[string]any, depth int, budget *scrubBudget) map[string]any {
	if depth > maxTemplateDepth || budget.nodes >= maxTemplateNodes {
		return map[string]any{"_redacted": true, "reason": "template_limit"}
	}
	keys := make([]string, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	truncated := false
	if len(keys) > 64 {
		keys = keys[:64]
		truncated = true
	}
	out := make(map[string]any, len(keys)+1)
	for _, key := range keys {
		budget.nodes++
		out[key] = scrubArgumentValue(strings.ToLower(key), source[key], depth+1, budget)
	}
	if truncated {
		out["_fieldsTruncated"] = true
	}
	return out
}

func scrubArgumentValue(key string, value any, depth int, budget *scrubBudget) any {
	if sensitiveArgumentKey(key) {
		return redactedDescriptor(value, "sensitive_field")
	}
	if strings.Contains(key, "url") {
		if raw, ok := value.(string); ok {
			return scrubURL(raw)
		}
	}
	if text, ok := value.(string); ok {
		if _, pathLike := pathTemplateKeys[key]; pathLike {
			return scrubPath(text)
		}
		switch key {
		case "package", "packagename":
			return scrubPackage(text)
		case "component":
			return scrubComponent(text)
		case "host", "hostname":
			return scrubHost(text)
		case "key", "jar":
			if safeIdentifier.MatchString(strings.ToLower(text)) {
				return boundedReusableString(text)
			}
			return redactedDescriptor(text, "unsafe_identifier")
		}
	}
	if depth > maxTemplateDepth || budget.nodes >= maxTemplateNodes {
		return redactedDescriptor(value, "template_limit")
	}
	switch typed := value.(type) {
	case map[string]any:
		return scrubObject(typed, depth, budget)
	case map[string]string:
		converted := make(map[string]any, len(typed))
		for nestedKey, item := range typed {
			converted[nestedKey] = item
		}
		return scrubObject(converted, depth, budget)
	case []any:
		limit := len(typed)
		if limit > 64 {
			limit = 64
		}
		items := make([]any, 0, limit+1)
		_, pathsReusable := pathArrayTemplateKeys[key]
		for index := 0; index < limit; index++ {
			budget.nodes++
			if text, ok := typed[index].(string); ok && pathsReusable {
				items = append(items, scrubPath(text))
			} else {
				items = append(items, scrubArgumentValue(key, typed[index], depth+1, budget))
			}
		}
		if len(typed) > limit {
			items = append(items, map[string]any{"redacted": true, "remainingItems": len(typed) - limit})
		}
		return items
	case []string:
		items := make([]any, len(typed))
		_, pathsReusable := pathArrayTemplateKeys[key]
		for index, item := range typed {
			if pathsReusable {
				items[index] = scrubPath(item)
			} else {
				items[index] = redactedDescriptor(item, "unclassified_string")
			}
		}
		return items
	case string:
		if _, allowed := reusableStringKeys[key]; allowed {
			return boundedReusableString(typed)
		}
		return redactedDescriptor(typed, "unclassified_string")
	case bool, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number, nil:
		return typed
	default:
		return redactedDescriptor(typed, "unsupported_type")
	}
}

func sensitiveArgumentKey(key string) bool {
	markers := []string{"authorization", "cookie", "token", "password", "passwd", "secret", "credential", "privatekey", "bearer", "api_key", "apikey", "stdin", "body", "content", "data", "headers", "env", "binarybase64", "message", "value", "text", "command", "cmd", "script", "query", "reason", "pattern", "subprotocol", "arguments", "args"}
	for _, marker := range markers {
		if strings.Contains(key, marker) {
			return true
		}
	}
	return false
}

func boundedReusableString(value string) any {
	if len(value) > 256 {
		return redactedDescriptor(value, "string_length")
	}
	return value
}

func redactedDescriptor(value any, reason string) map[string]any {
	raw, _ := json.Marshal(value)
	return map[string]any{"redacted": true, "type": fmt.Sprintf("%T", value), "bytes": len(raw), "reason": reason}
}

func scrubURL(raw string) any {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return map[string]any{"redacted": true, "type": "url", "bytes": len(raw)}
	}
	segments := 0
	for _, segment := range strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/") {
		if segment != "" {
			segments++
		}
	}
	return map[string]any{
		"redacted": parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "",
		"type":     "url", "scheme": strings.ToLower(parsed.Scheme), "host": scrubHost(parsed.Host),
		"pathSegments": segments, "queryPresent": parsed.RawQuery != "", "fragmentPresent": parsed.Fragment != "",
	}
}

func scrubPath(value string) map[string]any {
	cleaned := filepath.Clean(value)
	segments := 0
	for _, segment := range strings.FieldsFunc(filepath.ToSlash(cleaned), func(r rune) bool { return r == '/' }) {
		if segment != "" && segment != "." {
			segments++
		}
	}
	return map[string]any{"redacted": true, "type": "path", "absolute": filepath.IsAbs(cleaned), "segments": segments, "extension": safeArtifactType(filepath.Ext(cleaned))}
}

func scrubPackage(value string) any {
	value = strings.TrimSpace(value)
	parts := strings.Split(value, ".")
	if len(parts) < 2 || len(value) > 255 {
		return redactedDescriptor(value, "invalid_package")
	}
	for _, part := range parts {
		if part == "" || !safeIdentifier.MatchString(strings.ToLower(part)) {
			return redactedDescriptor(value, "invalid_package")
		}
	}
	return value
}

func scrubComponent(value string) any {
	parts := strings.SplitN(strings.TrimSpace(value), "/", 2)
	if len(parts) != 2 || parts[1] == "" {
		return redactedDescriptor(value, "invalid_component")
	}
	if _, ok := scrubPackage(parts[0]).(string); !ok || strings.ContainsAny(parts[1], " \t\r\n") {
		return redactedDescriptor(value, "invalid_component")
	}
	return value
}

func scrubHost(value string) map[string]any {
	host := value
	if parsed, err := url.Parse("//" + value); err == nil && parsed.Hostname() != "" {
		host = parsed.Hostname()
	}
	labels := 0
	for _, label := range strings.Split(strings.Trim(host, "[]."), ".") {
		if label != "" {
			labels++
		}
	}
	return map[string]any{"redacted": true, "type": "host", "labels": labels, "ipLiteral": strings.Contains(host, ":") || numericHost.MatchString(host), "portPresent": strings.Contains(value, ":")}
}

func safeArtifactType(extension string) string {
	extension = strings.ToLower(strings.TrimSpace(extension))
	allowed := map[string]struct{}{".apk": {}, ".apks": {}, ".zip": {}, ".tar": {}, ".gz": {}, ".xz": {}, ".7z": {}, ".json": {}, ".jsonl": {}, ".xml": {}, ".txt": {}, ".log": {}, ".png": {}, ".jpg": {}, ".jpeg": {}, ".webp": {}, ".pdf": {}, ".csv": {}, ".sha256": {}}
	if _, ok := allowed[extension]; ok {
		return extension
	}
	return "file"
}

func sanitizeArtifactTypes(values []string, legacy bool) []string {
	result := make([]string, 0, min(len(values), maxLearnedArtifactTypes))
	for _, value := range values {
		if len(result) >= maxLearnedArtifactTypes {
			break
		}
		extension := strings.ToLower(strings.TrimSpace(value))
		if legacy || !strings.HasPrefix(extension, ".") {
			extension = filepath.Ext(extension)
		}
		result = appendUniqueSorted(result, safeArtifactType(extension))
	}
	return result
}

func sanitizeLoadedEntry(entry Entry) Entry {
	entry.Title = truncateKnowledgeText(strings.TrimSpace(entry.Title), 256)
	entry.Summary = truncateKnowledgeText(strings.Join(strings.Fields(entry.Summary), " "), 1024)
	entry.LastCode = truncateKnowledgeText(entry.LastCode, 128)
	entry.LastStrategy = truncateKnowledgeText(entry.LastStrategy, 128)
	entry.LastErrorClass = truncateKnowledgeText(entry.LastErrorClass, 128)
	entry.LastFingerprint = truncateKnowledgeText(entry.LastFingerprint, 128)
	entry.LastKnownGoodCode = truncateKnowledgeText(entry.LastKnownGoodCode, 128)
	entry.LastKnownGoodStrategy = truncateKnowledgeText(entry.LastKnownGoodStrategy, 128)
	entry.LastArtifacts = sanitizeArtifactTypes(entry.LastArtifacts, false)
	entry.LastKnownGoodArtifacts = sanitizeArtifactTypes(entry.LastKnownGoodArtifacts, false)
	if safeIdentifier.MatchString(entry.Tool) && safeIdentifier.MatchString(entry.Action) {
		if entry.LastArguments != nil {
			entry.LastArguments = scrubArguments(entry.Tool, entry.Action, entry.LastArguments)
		}
		if entry.LastKnownGoodArguments != nil {
			entry.LastKnownGoodArguments = scrubArguments(entry.Tool, entry.Action, entry.LastKnownGoodArguments)
		}
	} else {
		entry.LastArguments = nil
		entry.LastKnownGoodArguments = nil
	}
	entry.Keywords = boundedStrings(entry.Keywords, 64, 128)
	entry.Triggers = boundedStrings(entry.Triggers, 16, 128)
	if len(entry.FailureClasses) > 32 {
		bounded := make(map[string]int, 32)
		keys := make([]string, 0, len(entry.FailureClasses))
		for key := range entry.FailureClasses {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys[:32] {
			bounded[truncateKnowledgeText(key, 128)] = entry.FailureClasses[key]
		}
		entry.FailureClasses = bounded
	}
	return entry
}

func boundedStrings(values []string, maximum, bytesPerItem int) []string {
	if len(values) > maximum {
		values = values[:maximum]
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = truncateKnowledgeText(strings.TrimSpace(value), bytesPerItem)
		if value != "" {
			result = appendUniqueSorted(result, value)
		}
	}
	return result
}

func appendUniqueSorted(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	values = append(values, value)
	sort.Strings(values)
	return values
}

func truncateKnowledgeText(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("file exceeds %d-byte regular-file limit", limit)
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("file exceeds %d-byte limit", limit)
	}
	return raw, nil
}

func verifyForgetState(state forgetState) bool {
	if state.Markers == nil || len(state.Checksum) != sha256.Size*2 {
		return false
	}
	raw, err := json.Marshal(state.Markers)
	return err == nil && digest(raw) == strings.ToLower(state.Checksum)
}

func sortReceiptFiles(files []receiptFile) {
	sort.SliceStable(files, func(i, j int) bool {
		left, right := files[i].data, files[j].data
		if left.Tool != right.Tool {
			return left.Tool < right.Tool
		}
		if left.Action != right.Action {
			return left.Action < right.Action
		}
		if left.Generation != right.Generation {
			return left.Generation < right.Generation
		}
		if (left.Order == 0) != (right.Order == 0) {
			return left.Order == 0
		}
		if left.Order != right.Order {
			return left.Order < right.Order
		}
		if !left.CompletedAt.Equal(right.CompletedAt) {
			return left.CompletedAt.Before(right.CompletedAt)
		}
		return left.ID < right.ID
	})
}

func firstHeading(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return "内置技能"
}

func firstParagraph(body string) string {
	paragraphs := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n")
	for _, paragraph := range paragraphs {
		text := strings.TrimSpace(paragraph)
		if text != "" && !strings.HasPrefix(text, "#") {
			text = strings.Join(strings.Fields(text), " ")
			if len([]rune(text)) > 140 {
				text = string([]rune(text)[:140]) + "…"
			}
			return text
		}
	}
	return "按需加载的内置技能。"
}

func splitWords(value string) []string {
	parts := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-')
	})
	seen := make(map[string]struct{})
	out := make([]string, 0, len(parts))
	add := func(part string) bool {
		if len([]rune(part)) < 2 {
			return true
		}
		if _, ok := seen[part]; ok {
			return true
		}
		seen[part] = struct{}{}
		out = append(out, part)
		return len(out) < 120
	}
	for _, part := range parts {
		if !add(part) {
			break
		}
		runes := []rune(part)
		allHan := len(runes) > 2
		for _, r := range runes {
			if !unicode.Is(unicode.Han, r) {
				allHan = false
				break
			}
		}
		if allHan {
			for width := 2; width <= 3; width++ {
				for start := 0; start+width <= len(runes); start++ {
					if !add(string(runes[start : start+width])) {
						return out
					}
				}
			}
		}
	}
	return out
}

func slug(value string) string {
	value = strings.ToLower(value)
	var builder strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
		} else if builder.Len() > 0 && !strings.HasSuffix(builder.String(), "-") {
			builder.WriteByte('-')
		}
	}
	return strings.Trim(builder.String(), "-")
}

func validID(id string) bool {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(id)))
	return clean == id && !strings.Contains(id, "..") && !filepath.IsAbs(id)
}

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func empty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
