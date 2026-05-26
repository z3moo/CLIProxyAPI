package management

import (
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
)

// GetUsageStats returns aggregated usage statistics in the 9router-compatible
// shape (totalRequests, totalPromptTokens, totalCompletionTokens, totalCost
// plus byProvider/byModel/byApiKey/byAuth groupings).
func (h *Handler) GetUsageStats(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	period := strings.TrimSpace(c.Query("period"))
	snap := usage.Default().Snapshot(period)

	// Sort each dimension by request count desc so dashboards show the busiest
	// rows first.
	sortByRequests(snap.ByProvider)
	sortByRequests(snap.ByModel)
	sortByRequests(snap.ByAPIKey)
	sortByRequests(snap.ByAuth)

	c.JSON(http.StatusOK, snap)
}

// ResetUsageStats clears all aggregator state.
func (h *Handler) ResetUsageStats(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	usage.Default().Reset()
	c.JSON(http.StatusOK, gin.H{"status": "reset"})
}

func sortByRequests(rows []usage.DimensionRecord) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Requests != rows[j].Requests {
			return rows[i].Requests > rows[j].Requests
		}
		if rows[i].Cost != rows[j].Cost {
			return rows[i].Cost > rows[j].Cost
		}
		return rows[i].Model < rows[j].Model
	})
}
