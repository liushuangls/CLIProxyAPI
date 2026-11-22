package management

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagehistory"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestParseUsageHistoryQuery(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	for _, suffix := range []string{
		"", "?auth_id=a&auth_index=b", "?auth_id=", "?auth_index=a&page=0", "?auth_index=a&page_size=101", "?auth_index=a&range=all",
		"?auth_index=a&from=bad&to=bad", "?auth_index=a&from=2026-11-12T00:00:00Z",
		"?auth_index=a&from=2026-11-13T00:00:00Z&to=2026-11-12T00:00:00Z",
		"?auth_index=a&from=2026-11-12T00:00:00Z&to=2026-11-13T00:00:00Z&range=7d",
	} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/"+suffix, nil)
		if _, err := parseUsageHistoryQuery(c, now); err == nil {
			t.Errorf("accepted invalid query %s", suffix)
		}
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/?auth_index=a&range=7d&page=2", nil)
	q, err := parseUsageHistoryQuery(c, now)
	if err != nil || q.Page != 2 || q.PageSize != 25 || !q.From.Equal(now.Add(-7*24*time.Hour)) || !q.To.Equal(now) {
		t.Fatalf("wrong window: %+v %v", q, err)
	}
}

func TestCredentialUsageHistoryHandler(t *testing.T) {
	store, err := usagehistory.Open(filepath.Join(t.TempDir(), "history.db"), 30)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if errClose := store.Close(); errClose != nil {
			t.Error(errClose)
		}
	}()
	manager := coreauth.NewManager(nil, nil, nil)
	auth, err := manager.Register(t.Context(), &coreauth.Auth{ID: "fixture", Provider: "codex", FileName: "fixture.json"})
	if err != nil {
		t.Fatal(err)
	}
	index := auth.EnsureIndex()
	if err = store.Append(usage.Record{RequestID: "test-execution", AuthIndex: index, Provider: "codex", Model: "test-model", RequestedAt: time.Now().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)
	h.SetUsageHistory(store)
	request := func(query string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/?"+query, nil)
		h.GetCredentialUsageHistory(c)
		return recorder
	}
	for range 2 {
		r := request("auth_index=" + index)
		if r.Code != 200 || ojson.Get(r.Body.String(), "totals.requests").Int() != 1 {
			t.Fatalf("read consumed or lost history: %d %s", r.Code, r.Body.String())
		}
	}
	if r := request("auth_id=fixture"); r.Code != 200 || ojson.Get(r.Body.String(), "auth_ids.0").String() != "fixture" || ojson.Get(r.Body.String(), "totals.requests").Int() != 1 {
		t.Fatalf("provider history: %d %s", r.Code, r.Body.String())
	}
	if r := request("auth_id=fixture&auth_id=missing"); r.Code != 404 {
		t.Fatalf("partial group accepted: %d", r.Code)
	}
	if r := request("auth_index=missing"); r.Code != 404 {
		t.Fatalf("missing auth returned %d", r.Code)
	}
	if r := request("auth_index=" + index + "&page=-1"); r.Code != 400 {
		t.Fatalf("invalid page returned %d", r.Code)
	}
	h.SetUsageHistory(nil)
	if r := request("auth_index=" + index); r.Code != 503 {
		t.Fatalf("disabled history returned %d", r.Code)
	}
}
