package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestCredentialUsageHistoryRequiresManagementAuthentication(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	cfg, err := config.ParseConfigBytes([]byte("management:\n  secret-key: test-only-password\n"))
	if err != nil {
		t.Fatal(err)
	}
	h := management.NewHandler(cfg, "", nil)
	h.SetLocalPassword("test-only-password")
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementV8Routes()
	for _, authorized := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/credentials?auth_index=test", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		want := 401
		if authorized {
			req.Header.Set("Authorization", "Bearer test-only-password")
			want = 503
		}
		r := httptest.NewRecorder()
		s.engine.ServeHTTP(r, req)
		if r.Code != want {
			t.Fatalf("authorized=%t: status=%d, want %d: %s", authorized, r.Code, want, r.Body.String())
		}
	}
}
