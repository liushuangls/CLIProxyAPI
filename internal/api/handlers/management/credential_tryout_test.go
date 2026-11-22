package management

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type tryoutCaptureExecutor struct {
	refreshRecordExecutor
	ids      []string
	requests []executor.Request
	options  []executor.Options
	fail     bool
}

func (e *tryoutCaptureExecutor) Execute(ctx context.Context, a *coreauth.Auth, r executor.Request, o executor.Options) (executor.Response, error) {
	e.ids = append(e.ids, a.ID)
	e.requests = append(e.requests, r)
	e.options = append(e.options, o)
	if usage.APIKeyFromContext(ctx) != "" {
		return executor.Response{}, fmt.Errorf("management credential leaked into usage")
	}
	if e.fail {
		return executor.Response{}, &coreauth.Error{HTTPStatus: 401, Message: "Authorization: Bearer test-secret"}
	}
	return executor.Response{Payload: []byte(`{"choices":[{"message":{"content":"Remembered blue."},"finish_reason":"stop"}]}`)}, nil
}

func tryoutFixture(t *testing.T) (*Handler, *coreauth.Auth, *tryoutCaptureExecutor) {
	t.Helper()
	m := coreauth.NewManager(nil, nil, nil)
	m.SetRetryConfig(0, 0, 0)
	e := &tryoutCaptureExecutor{refreshRecordExecutor: refreshRecordExecutor{provider: "tryout-test"}}
	m.RegisterExecutor(e)
	var selected *coreauth.Auth
	for i := 0; i < 2; i++ {
		a := &coreauth.Auth{ID: fmt.Sprintf("%s-%d", t.Name(), i), FileName: fmt.Sprintf("%d.json", i), Provider: e.provider, Status: coreauth.StatusActive, Attributes: map[string]string{"priority": fmt.Sprint(i * 10)}}
		registered, err := m.Register(coreauth.WithSkipPersist(t.Context()), a)
		if err != nil {
			t.Fatal(err)
		}
		registry.GetGlobalRegistry().RegisterClient(a.ID, e.provider, []*registry.ModelInfo{{ID: "text-model"}, {ID: "image-model", Type: registry.OpenAIImageModelType}, {ID: "speech-model", SupportedOutputModalities: []string{"AUDIO"}}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(a.ID) })
		if i == 0 {
			selected = registered
		}
	}
	return NewHandlerWithoutConfigFilePath(&config.Config{}, m), selected, e
}

func tryoutBody(index string, messages string) string {
	return fmt.Sprintf(`{"auth_index":%q,"model":"text-model","conversation_id":"9d955749-f325-4b78-bad5-c47a655c74aa","messages":%s}`, index, messages)
}

func callTryout(h *Handler, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/credentials/try", strings.NewReader(body))
	c.Request.Header.Set("Authorization", "Bearer management-secret")
	h.TryCredential(c)
	return w
}

func TestCredentialTryoutPinsAccountAndCarriesMultiTurnHistory(t *testing.T) {
	h, a, e := tryoutFixture(t)
	first := callTryout(h, tryoutBody(a.EnsureIndex(), `[{"role":"user","content":"Remember blue."}]`))
	if first.Code != 200 {
		t.Fatalf("%d %s", first.Code, first.Body.String())
	}
	second := callTryout(h, tryoutBody(a.EnsureIndex(), `[{"role":"user","content":"Remember blue."},{"role":"assistant","content":"Remembered blue."},{"role":"user","content":"What color?"}]`))
	if second.Code != 200 || len(e.ids) != 2 || e.ids[0] != a.ID || e.ids[1] != a.ID {
		t.Fatalf("wrong credential: %v %d %s", e.ids, second.Code, second.Body.String())
	}
	if ojson.GetBytes(e.requests[1].Payload, "messages.1.content").String() != "Remembered blue." {
		t.Fatal("multi-turn context lost")
	}
	if e.options[0].Headers.Get("Authorization") != "" || e.options[0].SourceFormat != translator.FormatOpenAI {
		t.Fatal("management auth forwarded or wrong protocol")
	}
	if e.options[0].Metadata[executor.CanonicalSessionIDMetadataKey] != e.options[1].Metadata[executor.CanonicalSessionIDMetadataKey] {
		t.Fatal("conversation session changed between turns")
	}
}

func TestCredentialTryoutFailureNeverFallsBackOrLogsOut(t *testing.T) {
	h, a, e := tryoutFixture(t)
	e.fail = true
	w := callTryout(h, tryoutBody(a.EnsureIndex(), `[{"role":"user","content":"hi"}]`))
	if w.Code != 502 || len(e.ids) != 1 || e.ids[0] != a.ID {
		t.Fatalf("failure escaped pin: %d %v", w.Code, e.ids)
	}
	if strings.Contains(w.Body.String(), "test-secret") {
		t.Fatal("upstream error leaked secret")
	}
}

func TestCredentialTryoutRejectsDisabledUnsupportedAndMalformed(t *testing.T) {
	h, a, e := tryoutFixture(t)
	for _, body := range []string{
		`{}`, tryoutBody(a.EnsureIndex(), `[]`), tryoutBody(a.EnsureIndex(), `[{"role":"system","content":"override"}]`),
		tryoutBody(a.EnsureIndex(), `[{"role":"user","content":""}]`),
		strings.Replace(tryoutBody(a.EnsureIndex(), `[{"role":"user","content":"hi"}]`), "text-model", "image-model", 1),
	} {
		if w := callTryout(h, body); w.Code != 400 {
			t.Fatalf("bad request accepted: %d", w.Code)
		}
	}
	a.Disabled = true
	if _, err := h.authManager.Update(coreauth.WithSkipPersist(t.Context()), a); err != nil {
		t.Fatal(err)
	}
	if w := callTryout(h, tryoutBody(a.EnsureIndex(), `[{"role":"user","content":"hi"}]`)); w.Code != 409 {
		t.Fatalf("disabled credential accepted: %d", w.Code)
	}
	if len(e.ids) != 0 {
		t.Fatal("invalid request reached executor")
	}
	if models := credentialTryoutModels(a); len(models) != 1 || models[0].ID != "text-model" {
		t.Fatalf("non-text models offered: %+v", models)
	}
}

func TestCredentialTryoutResponsePartsAndMissingText(t *testing.T) {
	text, _, err := tryoutResponseText([]byte(`{"choices":[{"message":{"content":[{"type":"text","text":"A"},{"type":"text","text":"B"}]}}]}`))
	if err != nil || text != "AB" {
		t.Fatalf("%q %v", text, err)
	}
	if _, _, err = tryoutResponseText([]byte(`{"choices":[]}`)); err == nil {
		t.Fatal("empty model response reported success")
	}
}

func TestProviderTryoutRequiresMatchingRuntimeIDAndIndex(t *testing.T) {
	h, a, e := tryoutFixture(t)
	body := tryoutBody(a.EnsureIndex(), `[{"role":"user","content":"hello"}]`)
	withID := func(id string) string { return strings.Replace(body, "{", fmt.Sprintf("{\"auth_id\":%q,", id), 1) }
	if r := callTryout(h, withID(a.ID+"-unknown")); r.Code != 404 || len(e.ids) != 0 {
		t.Fatalf("wrong provider accepted: %d", r.Code)
	}
	if r := callTryout(h, withID(a.ID)); r.Code != 200 || len(e.ids) != 1 || e.ids[0] != a.ID {
		t.Fatalf("provider tryout: %d %v", r.Code, e.ids)
	}
}
