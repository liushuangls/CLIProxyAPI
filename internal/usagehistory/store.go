// Package usagehistory persists credential-scoped usage without consuming the export queue.
package usagehistory

import (
	"bytes"
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	bolt "go.etcd.io/bbolt"
)

const DefaultRetentionDays = 30

var (
	rootBucket   = []byte("credential-usage-v1")
	authsBucket  = []byte("credentials")
	eventsBucket = []byte("events")
	idsBucket    = []byte("ids")
	startedKey   = []byte("started-at")
	retainedKey  = []byte("retained-since")
)

// Tokens contains canonical totals. Cache and reasoning are subsets, not extra tokens.
type Tokens struct {
	Input        int64 `json:"input"`
	Output       int64 `json:"output"`
	CacheRead    int64 `json:"cache_read"`
	CacheWrite   int64 `json:"cache_write"`
	Reasoning    int64 `json:"reasoning"`
	Total        int64 `json:"total"`
	Unclassified int64 `json:"unclassified"`
}

// Event deliberately excludes prompts, responses, API keys, and credential secrets.
type Event struct {
	AuthIDHash      string                       `json:"-"`
	ExecutionID     string                       `json:"execution_id"`
	TraceID         string                       `json:"trace_id"`
	Timestamp       time.Time                    `json:"timestamp"`
	Provider        string                       `json:"provider"`
	Model           string                       `json:"model"`
	Alias           string                       `json:"alias"`
	ResponseModel   string                       `json:"response_model"`
	AdditionalUsage bool                         `json:"additional_usage"`
	Failed          bool                         `json:"failed"`
	StatusCode      int                          `json:"status_code"`
	ErrorCode       string                       `json:"error_code"`
	LatencyMS       int64                        `json:"latency_ms"`
	Quality         usage.TokenAccountingQuality `json:"quality"`
	Tokens          Tokens                       `json:"tokens"`
}

type Stats struct {
	Requests         int64    `json:"requests"`
	Success          int64    `json:"success"`
	Failed           int64    `json:"failed"`
	SuccessRate      *float64 `json:"success_rate"`
	CacheHitRate     *float64 `json:"cache_hit_rate"`
	AdditionalEvents int64    `json:"additional_events"`
	UncertainEvents  int64    `json:"uncertain_events"`
	Tokens           Tokens   `json:"tokens"`
}

type ModelStats struct {
	Model string `json:"model"`
	Stats
}

// ProviderSelection restricts new records by exact runtime ID and older records
// (which have no runtime ID hash) by their recorded provider within the credential bucket.
type ProviderSelection struct {
	IDs                map[string]bool
	Providers          map[string]bool
	AmbiguousProviders map[string]bool
}

var ErrAmbiguousHistory = errors.New("older usage records share a credential index and provider and cannot be separated")

type Query struct {
	Selection   map[string]*ProviderSelection
	AuthIndexes []string
	From        time.Time
	To          time.Time
	Page        int
	PageSize    int
}

type Result struct {
	AuthIDs        []string     `json:"auth_ids,omitempty"`
	AuthIndex      string       `json:"auth_index,omitempty"`
	AuthIndexes    []string     `json:"auth_indexes,omitempty"`
	From           time.Time    `json:"from"`
	To             time.Time    `json:"to"`
	AvailableSince time.Time    `json:"available_since"`
	RetentionDays  int          `json:"retention_days"`
	Totals         Stats        `json:"totals"`
	Models         []ModelStats `json:"models"`
	Events         []Event      `json:"events"`
	Page           int          `json:"page"`
	PageSize       int          `json:"page_size"`
	TotalEvents    int          `json:"total_events"`
}

// Store owns one bbolt file. A write failure latches an error so readers cannot
// mistake a partial collection for a healthy, complete history.
type Store struct {
	db            *bolt.DB
	retentionDays int
	startedAt     time.Time
	now           func() time.Time
	mu            sync.Mutex
	writeErr      error
	stop          chan struct{}
	done          chan struct{}
	closeOnce     sync.Once
	closeErr      error
}

func Open(path string, retentionDays int) (*Store, error) {
	return openStore(path, retentionDays, time.Now)
}

func openStore(path string, retentionDays int, now func() time.Time) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("usage history path is empty")
	}
	if retentionDays == 0 {
		retentionDays = DefaultRetentionDays
	}
	if retentionDays < 1 || retentionDays > 365 {
		return nil, errors.New("usage history retention must be between 1 and 365 days")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create usage history directory: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("open usage history: %w", err)
	}
	s := &Store{db: db, retentionDays: retentionDays, now: now, stop: make(chan struct{}), done: make(chan struct{})}
	err = db.Update(func(tx *bolt.Tx) error {
		root, errRoot := tx.CreateBucketIfNotExists(rootBucket)
		if errRoot != nil {
			return errRoot
		}
		if _, errAuths := root.CreateBucketIfNotExists(authsBucket); errAuths != nil {
			return errAuths
		}
		started := root.Get(startedKey)
		if started != nil {
			var errTime error
			s.startedAt, errTime = time.Parse(time.RFC3339Nano, string(started))
			return errTime
		}
		s.startedAt = now().UTC()
		return root.Put(startedKey, []byte(s.startedAt.Format(time.RFC3339Nano)))
	})
	if err == nil {
		err = s.Prune()
	}
	if err != nil {
		if errClose := db.Close(); errClose != nil {
			err = errors.Join(err, errClose)
		}
		return nil, fmt.Errorf("initialize usage history: %w", err)
	}
	go s.cleanup()
	return s, nil
}

func (s *Store) cleanup() {
	defer close(s.done)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			if err := s.Prune(); err != nil {
				s.recordError(err)
			}
		}
	}
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

func (s *Store) recordError(err error) {
	s.mu.Lock()
	first := s.writeErr == nil
	if first {
		s.writeErr = err
	}
	s.mu.Unlock()
	if first {
		log.WithError(err).Error("usage history collection failed; history queries are unavailable until restart")
	}
}

func (s *Store) HandleUsage(_ context.Context, record usage.Record) {
	// The source request context can already be cancelled when the usage queue drains.
	if err := s.Append(record); err != nil {
		s.recordError(err)
	}
}

func (s *Store) cutoff() time.Time {
	return s.now().UTC().Add(-time.Duration(s.retentionDays) * 24 * time.Hour)
}

// RuntimeIDHash keeps file names and opaque credential identifiers out of stored events.
func RuntimeIDHash(id string) string {
	if id == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(id))
	return hex.EncodeToString(digest[:])
}

// Append commits one event; execution IDs make replay idempotent per credential.
func (s *Store) Append(record usage.Record) error {
	index := strings.TrimSpace(record.AuthIndex)
	if index == "" {
		return nil
	} // Failures before credential selection have no owner.
	if strings.TrimSpace(record.RequestID) == "" {
		return errors.New("usage history event has no execution ID")
	}
	at := record.RequestedAt.UTC()
	if at.IsZero() {
		at = s.now().UTC()
	}
	if at.Before(s.cutoff()) {
		return nil
	}
	detail := usage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType)
	b := detail.TokenBreakdown
	if !b.Valid() {
		return errors.New("usage history event has an invalid token breakdown")
	}
	event := Event{
		AuthIDHash:  RuntimeIDHash(record.AuthID),
		ExecutionID: record.RequestID, TraceID: record.TraceID, Timestamp: at,
		Provider: record.Provider, Model: record.Model, Alias: record.Alias, ResponseModel: record.ResponseModel,
		AdditionalUsage: record.AdditionalUsage || (record.HomeSelection != nil && record.HomeSelection.Additional),
		Failed:          record.Failed, StatusCode: record.Fail.StatusCode, LatencyMS: max(0, record.Latency.Milliseconds()),
		Quality: b.Quality,
		Tokens: Tokens{Input: b.Input.TotalTokens, Output: b.Output.TotalTokens,
			CacheRead: b.Input.CacheReadTokens, CacheWrite: b.Input.CacheWriteTokens,
			Reasoning: b.Output.ReasoningTokens, Total: b.TotalTokens, Unclassified: b.UnclassifiedTokens},
	}
	// Failure bodies and arbitrary plugin error text can contain credentials.
	switch record.Fail.Code {
	case usage.FailureStreamTruncatedCode, usage.FailureRequestScopedCode, usage.FailureForceCooldownCode:
		event.ErrorCode = record.Fail.Code
	}
	var encoded bytes.Buffer
	if err := gob.NewEncoder(&encoded).Encode(event); err != nil {
		return fmt.Errorf("encode usage event: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		auths := tx.Bucket(rootBucket).Bucket(authsBucket)
		auth, err := auths.CreateBucketIfNotExists([]byte(index))
		if err != nil {
			return err
		}
		events, err := auth.CreateBucketIfNotExists(eventsBucket)
		if err != nil {
			return err
		}
		ids, err := auth.CreateBucketIfNotExists(idsBucket)
		if err != nil {
			return err
		}
		id := []byte(event.ExecutionID)
		if ids.Get(id) != nil {
			return nil
		}
		key := append(timeKey(at), id...)
		if err = events.Put(key, encoded.Bytes()); err != nil {
			return err
		}
		return ids.Put(id, key)
	})
}

func timeKey(at time.Time) []byte {
	return binary.BigEndian.AppendUint64(nil, uint64(at.UnixNano()))
}

// Prune removes expired events and their deduplication keys in the same transaction.
func (s *Store) Prune() error {
	retainedSince := s.cutoff()
	return s.db.Update(func(tx *bolt.Tx) error {
		root := tx.Bucket(rootBucket)
		if raw := root.Get(retainedKey); raw != nil {
			previous, err := time.Parse(time.RFC3339Nano, string(raw))
			if err != nil {
				return fmt.Errorf("read history retention boundary: %w", err)
			}
			if previous.After(retainedSince) {
				retainedSince = previous
			}
		}
		// Extending retention cannot restore records pruned under an older setting.
		if err := root.Put(retainedKey, []byte(retainedSince.Format(time.RFC3339Nano))); err != nil {
			return err
		}
		cutoff := timeKey(retainedSince)
		auths := root.Bucket(authsBucket)
		return auths.ForEachBucket(func(index []byte) error {
			auth := auths.Bucket(index)
			events, ids := auth.Bucket(eventsBucket), auth.Bucket(idsBucket)
			cursor := events.Cursor()
			for key, _ := cursor.First(); key != nil && bytes.Compare(key, cutoff) < 0; key, _ = cursor.Next() {
				if len(key) < 8 {
					return errors.New("invalid usage history event key")
				}
				if err := ids.Delete(key[8:]); err != nil {
					return err
				}
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

// Each cursor contributes its newest remaining event. Break identical event-key
// ties by credential so page boundaries stay stable across requests.
type eventCursor struct {
	cursor     *bolt.Cursor
	key, value []byte
	index      string
}
type eventCursors []eventCursor

func (c eventCursors) Len() int { return len(c) }
func (c eventCursors) Less(i, j int) bool {
	if cmp := bytes.Compare(c[i].key, c[j].key); cmp != 0 {
		return cmp > 0
	}
	return c[i].index < c[j].index
}
func (c eventCursors) Swap(i, j int)   { c[i], c[j] = c[j], c[i] }
func (c *eventCursors) Push(value any) { *c = append(*c, value.(eventCursor)) }
func (c *eventCursors) Pop() any {
	last := len(*c) - 1
	value := (*c)[last]
	*c = (*c)[:last]
	return value
}

// Query uses a single read transaction for consistent totals and newest-first pagination.
// Time windows are [from, to); cached/reasoning tokens are never added to totals twice.
func (s *Store) Query(ctx context.Context, q Query) (Result, error) {
	if len(q.AuthIndexes) == 0 || !q.From.Before(q.To) || q.From.UnixNano() < 0 || q.Page < 1 || q.Page > 1000000 || q.PageSize < 1 || q.PageSize > 100 {
		return Result{}, errors.New("invalid usage history query")
	}
	s.mu.Lock()
	writeErr := s.writeErr
	s.mu.Unlock()
	if writeErr != nil {
		return Result{}, fmt.Errorf("usage history collection is incomplete: %w", writeErr)
	}
	available := s.cutoff()
	if s.startedAt.After(available) {
		available = s.startedAt
	}
	indexes := slices.Clone(q.AuthIndexes)
	slices.Sort(indexes)
	indexes = slices.Compact(indexes)
	if indexes[0] == "" {
		return Result{}, errors.New("empty credential index")
	}
	out := Result{From: q.From.UTC(), To: q.To.UTC(), AvailableSince: available,
		RetentionDays: s.retentionDays, Models: []ModelStats{}, Events: []Event{}, Page: q.Page, PageSize: q.PageSize}
	if len(indexes) == 1 {
		out.AuthIndex = indexes[0]
	} else {
		out.AuthIndexes = indexes
	}
	from := q.From
	// Retention is enforced on reads as well as by the hourly disk cleanup.
	if cutoff := s.cutoff(); from.Before(cutoff) {
		from = cutoff
	}
	models := make(map[string]*ModelStats)
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		root := tx.Bucket(rootBucket)
		retainedSince, err := time.Parse(time.RFC3339Nano, string(root.Get(retainedKey)))
		if err != nil {
			return fmt.Errorf("read history retention boundary: %w", err)
		}
		if retainedSince.After(out.AvailableSince) {
			out.AvailableSince = retainedSince
		}
		if retainedSince.After(from) {
			from = retainedSince
		}
		if !from.Before(q.To) {
			return nil
		}
		lower, upper := timeKey(from), timeKey(q.To)
		var cursors eventCursors
		for _, index := range indexes {
			auth := root.Bucket(authsBucket).Bucket([]byte(index))
			if auth == nil {
				continue
			}
			cursor := auth.Bucket(eventsBucket).Cursor()
			key, value := cursor.Seek(upper)
			if key == nil {
				key, value = cursor.Last()
			} else {
				key, value = cursor.Prev()
			}
			if bytes.Compare(key, lower) >= 0 {
				cursors = append(cursors, eventCursor{cursor: cursor, key: key, value: value, index: index})
			}
		}
		heap.Init(&cursors)
		offset := (q.Page - 1) * q.PageSize
		for len(cursors) > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			var event Event
			if err := gob.NewDecoder(bytes.NewReader(cursors[0].value)).Decode(&event); err != nil {
				return fmt.Errorf("decode usage history event: %w", err)
			}
			selection := q.Selection[cursors[0].index]
			if selection != nil && event.AuthIDHash == "" && selection.AmbiguousProviders[event.Provider] {
				return ErrAmbiguousHistory
			}
			if selection == nil || selection.IDs[event.AuthIDHash] || (event.AuthIDHash == "" && selection.Providers[event.Provider]) {
				if out.TotalEvents >= offset && len(out.Events) < q.PageSize {
					out.Events = append(out.Events, event)
				}
				out.TotalEvents++
				out.Totals.add(event)
				model := models[event.Model]
				if model == nil {
					model = &ModelStats{Model: event.Model}
					models[event.Model] = model
				}
				model.Stats.add(event)
			}
			head := &cursors[0]
			head.key, head.value = head.cursor.Prev()
			if bytes.Compare(head.key, lower) < 0 {
				heap.Pop(&cursors)
			} else {
				heap.Fix(&cursors, 0)
			}
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	out.Totals.finish()
	for _, model := range models {
		model.Stats.finish()
		out.Models = append(out.Models, *model)
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].Model < out.Models[j].Model })
	return out, nil
}

func (s *Stats) add(e Event) {
	if e.AdditionalUsage {
		s.AdditionalEvents++
	} else {
		s.Requests++
		if e.Failed {
			s.Failed++
		} else {
			s.Success++
		}
	}
	if e.Quality != usage.TokenAccountingQualityComplete {
		s.UncertainEvents++
	}
	s.Tokens.Input += e.Tokens.Input
	s.Tokens.Output += e.Tokens.Output
	s.Tokens.CacheRead += e.Tokens.CacheRead
	s.Tokens.CacheWrite += e.Tokens.CacheWrite
	s.Tokens.Reasoning += e.Tokens.Reasoning
	s.Tokens.Total += e.Tokens.Total
	s.Tokens.Unclassified += e.Tokens.Unclassified
}

func (s *Stats) finish() {
	if s.Tokens.Input > 0 {
		rate := float64(s.Tokens.CacheRead) * 100 / float64(s.Tokens.Input)
		s.CacheHitRate = &rate
	}
	if s.Requests > 0 {
		rate := float64(s.Success) * 100 / float64(s.Requests)
		s.SuccessRate = &rate
	}
}
