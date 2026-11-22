package management

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type tryoutMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type tryoutRequest struct {
	AuthID         string
	AuthIndex      string
	Model          string
	ConversationID uuid.UUID
	Messages       []tryoutMessage
}

type tryoutModel struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// credentialTryoutModels retains only models exposed by the selected credential.
func credentialTryoutModels(auth *coreauth.Auth) []tryoutModel {
	models := []tryoutModel{}
	for _, model := range registry.GetGlobalRegistry().GetModelsForClient(auth.ID) {
		if model == nil || model.ID == "" || model.Type == registry.OpenAIImageModelType {
			continue
		}
		if len(model.SupportedOutputModalities) > 0 && !slices.ContainsFunc(model.SupportedOutputModalities, func(s string) bool { return strings.EqualFold(s, "text") }) {
			continue
		}
		label := model.DisplayName
		if label == "" {
			label = model.ID
		}
		models = append(models, tryoutModel{ID: model.ID, Label: label})
	}
	slices.SortFunc(models, func(a, b tryoutModel) int { return strings.Compare(a.ID, b.ID) })
	return models
}

func (h *Handler) tryoutAuth(c *gin.Context, index, id string) *coreauth.Auth {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "credential_manager_unavailable"})
		return nil
	}
	if index == "" || len(index) > 256 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_tryout", "message": "auth_index is required"})
		return nil
	}
	var auth *coreauth.Auth
	var ok bool
	if id == "" {
		auth, ok = h.lookupAuthFile("", index)
	} else {
		auth, ok = h.authManager.GetByID(id)
		ok = ok && lockedAuthIndex(auth) == index
	}
	if !ok || auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "credential_not_found"})
		return nil
	}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		c.JSON(http.StatusConflict, gin.H{"error": "credential_disabled", "message": "Enable this credential before trying it."})
		return nil
	}
	return auth
}

func (h *Handler) GetCredentialTryoutModels(c *gin.Context) {
	auth := h.tryoutAuth(c, strings.TrimSpace(c.Query("auth_index")), c.Query("auth_id"))
	if auth == nil {
		return
	}
	c.JSON(http.StatusOK, gin.H{"models": credentialTryoutModels(auth)})
}

func parseTryoutRequest(raw []byte) (tryoutRequest, error) {
	var out tryoutRequest
	if !ojson.ValidBytes(raw) {
		return out, fmt.Errorf("invalid JSON body")
	}
	root := ojson.ParseBytes(raw)
	if !root.IsObject() {
		return out, fmt.Errorf("request body must be an object")
	}
	for _, field := range []string{"auth_index", "model", "conversation_id"} {
		if root.Get(field).Type != ojson.String {
			return out, fmt.Errorf("%s must be a string", field)
		}
	}
	if id := root.Get("auth_id"); id.Exists() {
		if id.Type != ojson.String || strings.TrimSpace(id.String()) == "" {
			return out, fmt.Errorf("invalid auth_id")
		}
		out.AuthID = id.String()
	}
	out.AuthIndex = strings.TrimSpace(root.Get("auth_index").String())
	out.Model = strings.TrimSpace(root.Get("model").String())
	var err error
	out.ConversationID, err = uuid.Parse(root.Get("conversation_id").String())
	if err != nil || out.ConversationID == uuid.Nil {
		return out, fmt.Errorf("conversation_id must be a nonzero UUID")
	}
	if out.AuthIndex == "" || len(out.AuthIndex) > 256 || out.Model == "" || len(out.Model) > 512 {
		return out, fmt.Errorf("auth_index and model are required")
	}
	list := root.Get("messages")
	if !list.IsArray() {
		return out, fmt.Errorf("messages must be an array")
	}
	items := list.Array()
	if len(items) == 0 || len(items) > 39 || len(items)%2 != 1 {
		return out, fmt.Errorf("send at most 20 turns, ending with a user message")
	}
	total := 0
	for i, item := range items {
		role, content := item.Get("role"), item.Get("content")
		expected := "user"
		if i%2 == 1 {
			expected = "assistant"
		}
		if !item.IsObject() || role.Type != ojson.String || role.String() != expected || content.Type != ojson.String || strings.TrimSpace(content.String()) == "" {
			return out, fmt.Errorf("messages must alternate nonempty user and assistant text")
		}
		count := utf8.RuneCountInString(content.String())
		if role.String() == "user" && count > 4000 {
			return out, fmt.Errorf("user messages are limited to 4000 characters")
		}
		total += count
		if total > 32000 {
			return out, fmt.Errorf("conversation exceeds 32000 characters; start a new conversation")
		}
		out.Messages = append(out.Messages, tryoutMessage{Role: expected, Content: content.String()})
	}
	return out, nil
}

// TryCredential uses the normal executor/accounting pipeline with an explicit pin.
// It does not forward management credentials or accept arbitrary upstream URLs.
func (h *Handler) TryCredential(c *gin.Context) {
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 256<<10))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_tryout", "message": "request body exceeds 256 KiB or could not be read"})
		return
	}
	body, err := parseTryoutRequest(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_tryout", "message": err.Error()})
		return
	}
	auth := h.tryoutAuth(c, body.AuthIndex, body.AuthID)
	if auth == nil {
		return
	}
	if !slices.ContainsFunc(credentialTryoutModels(auth), func(m tryoutModel) bool { return m.ID == body.Model }) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tryout_model_unavailable", "message": "Choose a text model offered by this credential."})
		return
	}
	doc := ojson.NewDocString(`{}`)
	for _, field := range []struct {
		name  string
		value any
	}{
		{"model", body.Model}, {"messages", body.Messages}, {"max_tokens", 1024}, {"stream", false},
	} {
		if err = doc.Set(field.name, field.value); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "tryout_encoding_failed"})
			return
		}
	}
	encoded := doc.Bytes()
	// Isolate trial conversations from normal clients and from other credentials.
	sessionID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("credential-tryout/"+auth.ID+"/"+body.ConversationID.String())).String()
	ctx := usage.WithCapturedIdentity(c.Request.Context(), "", "management-tryout", false)
	ctx = usage.WithRequestedModelAlias(ctx, body.Model)
	start := time.Now()
	response, err := h.authManager.Execute(ctx, []string{auth.Provider}, executor.Request{Model: body.Model, Payload: encoded}, executor.Options{
		SourceFormat: translator.FormatOpenAI, ResponseFormat: translator.FormatOpenAI, OriginalRequest: encoded,
		Headers: http.Header{"Content-Type": {"application/json"}, "X-Session-Id": {sessionID}},
		Metadata: map[string]any{
			executor.PinnedAuthMetadataKey:         auth.ID,
			executor.RequestedModelMetadataKey:     body.Model,
			executor.CanonicalSessionIDMetadataKey: sessionID,
			executor.ExecutionSessionMetadataKey:   sessionID,
			executor.RequestPathMetadataKey:        "/v8/management/credentials/try",
		},
	})
	if c.Request.Context().Err() != nil {
		return
	}
	if err != nil {
		// An upstream 401 is not a failed management login; avoid logging the UI out.
		c.JSON(http.StatusBadGateway, gin.H{"error": "credential_try_failed", "message": coreauth.SanitizeRenewalMessage(err.Error()), "upstream_status": clienterror.HTTPStatusFromErrorOr(err, http.StatusBadGateway)})
		return
	}
	text, finish, err := tryoutResponseText(response.Payload)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "tryout_no_text", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"auth_index": body.AuthIndex, "model": body.Model, "text": text, "finish_reason": finish, "elapsed_ms": time.Since(start).Milliseconds()})
}

func tryoutResponseText(raw []byte) (string, string, error) {
	if !ojson.ValidBytes(raw) {
		return "", "", fmt.Errorf("model returned an invalid response")
	}
	choice := ojson.GetBytes(raw, "choices.0")
	content := choice.Get("message.content")
	var text strings.Builder
	if content.Type == ojson.String {
		text.WriteString(content.String())
	} else if content.IsArray() {
		for _, part := range content.Array() {
			if part.Get("type").String() == "text" && part.Get("text").Type == ojson.String {
				text.WriteString(part.Get("text").String())
			}
		}
	}
	if text.Len() == 0 && choice.Get("message.refusal").Type == ojson.String {
		text.WriteString(choice.Get("message.refusal").String())
	}
	if strings.TrimSpace(text.String()) == "" {
		return "", "", fmt.Errorf("model did not return text; try another text model or start a new conversation")
	}
	return text.String(), choice.Get("finish_reason").String(), nil
}
