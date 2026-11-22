package usagehistory

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	bolt "go.etcd.io/bbolt"
)

func historyFixture(t *testing.T, retention int) (*Store, *atomic.Int64, string) {
	t.Helper()
	clock := new(atomic.Int64)
	clock.Store(time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC).UnixNano())
	path := filepath.Join(t.TempDir(), "usage.db")
	store, err := openStore(path, retention, func() time.Time { return time.Unix(0, clock.Load()).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if errClose := store.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	return store, clock, path
}

func sampleEvent(id, auth string, at time.Time) usage.Record {
	return usage.Record{RequestID: id, AuthIndex: auth, Model: "codex-model", Provider: "codex", RequestedAt: at,
		Detail: usage.Detail{TokenBreakdown: usage.NewSubsetTokenBreakdown(100, 40, 10, 50, 20, 150)}}
}

func queryFixture(store *Store, auth string) Query {
	return Query{AuthIndexes: []string{auth}, From: store.now().Add(-30 * 24 * time.Hour), To: store.now().Add(time.Second), Page: 1, PageSize: 25}
}

func TestHistoryAccountingIsolationDeduplicationAndPagination(t *testing.T) {
	store, _, _ := historyFixture(t, 30)
	at := store.now()
	first := sampleEvent("one", "auth-a", at)
	failure := sampleEvent("two", "auth-a", at.Add(time.Millisecond))
	failure.Failed, failure.Fail = true, usage.Failure{StatusCode: 502, Code: usage.FailureStreamTruncatedCode}
	failure.Detail = usage.Detail{}
	additional := sampleEvent("three", "auth-a", at.Add(2*time.Millisecond))
	additional.Model, additional.AdditionalUsage = "image-model", true
	for _, event := range []usage.Record{first, first, failure, additional, sampleEvent("one", "auth-b", at)} {
		if err := store.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	query := queryFixture(store, "auth-a")
	query.PageSize = 2
	result, err := store.Query(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	if result.Totals.Requests != 2 || result.Totals.Success != 1 || result.Totals.Failed != 1 || result.Totals.AdditionalEvents != 1 || *result.Totals.SuccessRate != 50 {
		t.Fatalf("incorrect attempt accounting: %+v", result.Totals)
	}
	if result.Totals.Tokens.Total != 300 || result.Totals.Tokens.Input != 200 || result.Totals.Tokens.Output != 100 || result.Totals.Tokens.CacheRead != 80 || result.Totals.Tokens.CacheWrite != 20 || result.Totals.Tokens.Reasoning != 40 {
		t.Fatalf("cache or reasoning counted twice: %+v", result.Totals.Tokens)
	}
	if result.TotalEvents != 3 || len(result.Events) != 2 || result.Events[0].ExecutionID != "three" || result.Events[1].ErrorCode != usage.FailureStreamTruncatedCode {
		t.Fatalf("incorrect newest-first page: %+v", result)
	}
	if len(result.Models) != 2 || result.Models[0].Model != "codex-model" || result.Models[1].SuccessRate != nil || result.Models[1].Requests != 0 {
		t.Fatalf("additional usage inflated model success rate: %+v", result.Models)
	}
	query.Page = 2
	page2, err := store.Query(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Events) != 1 || page2.Events[0].ExecutionID != "one" || page2.Totals.Requests != 2 {
		t.Fatalf("incorrect second page: %+v", page2)
	}
	other, err := store.Query(t.Context(), queryFixture(store, "auth-b"))
	if err != nil || other.Totals.Requests != 1 {
		t.Fatalf("credential histories mixed: %+v, %v", other, err)
	}
}

func TestHistoryPersistsAndPrunesConsecutiveExpiredRecords(t *testing.T) {
	store, clock, path := historyFixture(t, 1)
	started := store.startedAt
	for i := range 4 {
		if err := store.Append(sampleEvent(fmt.Sprint(i), "a", store.now().Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(path, 1, func() time.Time { return time.Unix(0, clock.Load()).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if errClose := reopened.Close(); errClose != nil {
			t.Error(errClose)
		}
	}()
	q := queryFixture(reopened, "a")
	q.To = q.To.Add(time.Minute)
	result, err := reopened.Query(t.Context(), q)
	if err != nil || result.TotalEvents != 4 || !reopened.startedAt.Equal(started) {
		t.Fatalf("lost persisted history: %+v, %v", result, err)
	}
	clock.Add(int64(24*time.Hour + 2*time.Second))
	q = queryFixture(reopened, "a")
	result, err = reopened.Query(t.Context(), q)
	if err != nil || result.TotalEvents != 2 {
		t.Fatalf("read retention not enforced: %+v, %v", result, err)
	}
	if err = reopened.Prune(); err != nil {
		t.Fatal(err)
	}
	err = reopened.db.View(func(tx *bolt.Tx) error {
		auth := tx.Bucket(rootBucket).Bucket(authsBucket).Bucket([]byte("a"))
		if auth.Bucket(eventsBucket).Stats().KeyN != 2 || auth.Bucket(idsBucket).Stats().KeyN != 2 {
			t.Fatal("expired events or deduplication keys remain")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHistoryWindowBoundariesAndEmptySuccessRate(t *testing.T) {
	store, _, _ := historyFixture(t, 30)
	at := store.now()
	for i := range 3 {
		if err := store.Append(sampleEvent(fmt.Sprint(i), "a", at.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	q := Query{AuthIndexes: []string{"a"}, From: at, To: at.Add(time.Second), Page: 1, PageSize: 25}
	result, err := store.Query(t.Context(), q)
	if err != nil || result.TotalEvents != 1 || result.Events[0].ExecutionID != "0" {
		t.Fatalf("window is not [from,to): %+v, %v", result, err)
	}
	q.AuthIndexes = []string{"missing"}
	result, err = store.Query(t.Context(), q)
	if err != nil || result.Totals.SuccessRate != nil || result.Events == nil || result.Models == nil {
		t.Fatalf("empty result must not invent success: %+v, %v", result, err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = store.Query(cancelled, q); err == nil {
		t.Fatal("cancelled query succeeded")
	}
}

func TestHistoryDoesNotPersistSecrets(t *testing.T) {
	store, _, path := historyFixture(t, 30)
	record := sampleEvent("one", "a", store.now())
	record.APIKey, record.Source, record.AccessTokenSHA256 = "secret-api-key", "secret-source", "secret-token-hash"
	record.Fail.Body, record.Fail.Code = "secret-response-body", "secret-untrusted-error"
	if err := store.Append(record); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{record.APIKey, record.Source, record.AccessTokenSHA256, record.Fail.Body, record.Fail.Code} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("sensitive field persisted: %q", secret)
		}
	}
}

func TestHistoryCollectionFailureIsVisible(t *testing.T) {
	store, _, _ := historyFixture(t, 30)
	store.HandleUsage(t.Context(), sampleEvent("", "a", store.now()))
	if _, err := store.Query(t.Context(), queryFixture(store, "a")); err == nil {
		t.Fatal("failed collection returned healthy empty statistics")
	}
}

func TestHistoryConcurrentReplayIsIdempotent(t *testing.T) {
	store, _, _ := historyFixture(t, 30)
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Go(func() {
			if err := store.Append(sampleEvent(fmt.Sprint(i%10), "a", store.now())); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	result, err := store.Query(t.Context(), queryFixture(store, "a"))
	if err != nil || result.TotalEvents != 10 {
		t.Fatalf("concurrent replay double-counted: %d, %v", result.TotalEvents, err)
	}
}

func TestProviderHistoryMergesPagesAndFiltersSharedIndexes(t *testing.T) {
	store, _, _ := historyFixture(t, 30)
	at := store.now()
	events := []struct{ idx, id, provider string }{
		{"a", "old-other", "other"}, {"a", "", "provider-a"}, {"b", "runtime-b", "provider-b"}, {"a", "other-id", "provider-a"}, {"a", "runtime-a", "provider-a"},
	}
	for i, sample := range events {
		r := sampleEvent(fmt.Sprint(i), sample.idx, at.Add(time.Duration(i)*time.Millisecond))
		r.AuthID, r.Provider = sample.id, sample.provider
		if err := store.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	q := queryFixture(store, "a")
	q.AuthIndexes = []string{"b", "a", "a"}
	q.Selection = map[string]*ProviderSelection{
		"a": {IDs: map[string]bool{RuntimeIDHash("runtime-a"): true}, Providers: map[string]bool{"provider-a": true}},
		"b": {IDs: map[string]bool{RuntimeIDHash("runtime-b"): true}, Providers: map[string]bool{"provider-b": true}},
	}
	q.PageSize = 2
	page, err := store.Query(t.Context(), q)
	if err != nil || page.TotalEvents != 3 || page.Totals.Requests != 3 || len(page.Models) != 1 || len(page.Events) != 2 || page.Events[0].ExecutionID != "4" || page.Events[1].ExecutionID != "2" {
		t.Fatalf("page one: %+v, %v", page, err)
	}
	q.Page = 2
	page, err = store.Query(t.Context(), q)
	if err != nil || len(page.Events) != 1 || page.Events[0].ExecutionID != "1" {
		t.Fatalf("page two: %+v, %v", page, err)
	}
	q.Selection["a"].AmbiguousProviders = map[string]bool{"provider-a": true}
	if _, err = store.Query(t.Context(), q); err != ErrAmbiguousHistory {
		t.Fatalf("ambiguous legacy record: %v", err)
	}
}
