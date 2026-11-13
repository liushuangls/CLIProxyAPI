package management

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagehistory"
	log "github.com/sirupsen/logrus"
)

func (h *Handler) SetUsageHistory(store *usagehistory.Store) {
	h.mu.Lock()
	h.usageHistory = store
	h.mu.Unlock()
}

// GetCredentialUsageHistory reads retained events without draining the export queue.
func (h *Handler) GetCredentialUsageHistory(c *gin.Context) {
	q, err := parseUsageHistoryQuery(c, time.Now().UTC())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_usage_query", "message": err.Error()})
		return
	}
	h.mu.Lock()
	store, manager := h.usageHistory, h.authManager
	h.mu.Unlock()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage_history_unavailable", "message": "Credential usage history is disabled or not running; check the history configuration and restart the server."})
		return
	}
	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "credential_manager_unavailable"})
		return
	}
	if _, ok := h.lookupAuthFile("", q.AuthIndex); !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "credential_not_found"})
		return
	}
	result, err := store.Query(c.Request.Context(), q)
	if err != nil {
		log.WithError(err).Error("query credential usage history")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "usage_history_incomplete", "message": "Usage history could not be read reliably. Check server logs before restarting."})
		return
	}
	c.JSON(http.StatusOK, result)
}

func parseUsageHistoryQuery(c *gin.Context, now time.Time) (usagehistory.Query, error) {
	q := usagehistory.Query{AuthIndex: strings.TrimSpace(c.Query("auth_index")), Page: 1, PageSize: 25, To: now}
	if q.AuthIndex == "" || len(q.AuthIndex) > 256 {
		return q, fmt.Errorf("auth_index is required (maximum 256 characters)")
	}
	from, to := c.Query("from"), c.Query("to")
	if from != "" || to != "" {
		if from == "" || to == "" || c.Query("range") != "" {
			return q, fmt.Errorf("provide both from and to, or range, but not both")
		}
		var err error
		q.From, err = time.Parse(time.RFC3339Nano, from)
		if err != nil {
			return q, fmt.Errorf("from must be an RFC3339 timestamp")
		}
		q.To, err = time.Parse(time.RFC3339Nano, to)
		if err != nil {
			return q, fmt.Errorf("to must be an RFC3339 timestamp")
		}
		if !q.From.Before(q.To) || q.From.Before(time.Unix(0, 0)) || q.To.After(now.Add(time.Minute)) || q.To.Sub(q.From) > 365*24*time.Hour {
			return q, fmt.Errorf("invalid time window (maximum 365 days; future windows are not supported)")
		}
	} else {
		var window time.Duration
		switch c.Query("range") {
		case "", "24h":
			window = 24 * time.Hour
		case "7d":
			window = 7 * 24 * time.Hour
		case "30d":
			window = 30 * 24 * time.Hour
		default:
			return q, fmt.Errorf("range must be 24h, 7d, or 30d")
		}
		q.From = now.Add(-window)
	}
	for _, field := range []struct {
		name    string
		target  *int
		maximum int
	}{
		{"page", &q.Page, 1000000}, {"page_size", &q.PageSize, 100},
	} {
		if raw := c.Query(field.name); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 1 || value > field.maximum {
				return q, fmt.Errorf("%s must be between 1 and %d", field.name, field.maximum)
			}
			*field.target = value
		}
	}
	return q, nil
}
