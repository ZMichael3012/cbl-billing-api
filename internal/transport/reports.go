// Package transport contains the HTTP transport of the billing-api service.
package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ZMichael3012/cbl-kit/ginmw"

	"github.com/cbl/cbl-billing-api/internal/reporting"
	"github.com/cbl/cbl-billing-api/internal/transport/httpmw"
)

// Merchant API report limits (openapi/billing.v1.yaml).
const (
	defaultReportLimit = 20
	maxReportLimit     = 100
	maxGroupByLength   = 64
	maxCursorLength    = 512
)

// reportTypePattern restricts the report type name in the path.
var reportTypePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// ReportsClient describes the reports client (implemented by internal/reporting).
type ReportsClient interface {
	Query(ctx context.Context, query reporting.Query) (reporting.Page, error)
}

// reportPage describes the report page response (the contract ReportPage).
type reportPage struct {
	// Rows sets the page rows.
	Rows []reporting.Row `json:"rows"`
	// NextCursor sets the next page cursor.
	NextCursor string `json:"next_cursor,omitempty"`
}

// RegisterReports registers GET /v1/reports/{report_type}: the merchant is
// always taken from the token (7.2), parameters are validated, and Reporting
// errors are translated into problem+json (404 unknown_report_type,
// 400 validation_failed, 503 service_unavailable).
func RegisterReports(group *gin.RouterGroup, client ReportsClient) {
	group.GET("/reports/:report_type", func(c *gin.Context) {
		handleReport(c, client)
	})
}

// handleReport serves a single report request.
func handleReport(c *gin.Context, client ReportsClient) {
	reportType := c.Param("report_type")
	if !reportTypePattern.MatchString(reportType) {
		abortUnknownReport(c)
		return
	}
	query, err := parseReportQuery(c, reportType)
	if err != nil {
		ginmw.AbortWithProblem(c, http.StatusBadRequest, "validation_failed",
			"Validation Failed", err.Error())
		return
	}
	merchantID := httpmw.MerchantID(c)
	if merchantID == "" {
		// The route is outside the Require group: a configuration error, not data.
		abortUnavailable(c)
		return
	}
	if client == nil {
		abortUnavailable(c)
		return
	}
	page, err := client.Query(c.Request.Context(), query)
	if err != nil {
		switch {
		case errors.Is(err, reporting.ErrUnknownReportType):
			ginmw.AbortWithProblem(c, http.StatusNotFound, "unknown_report_type",
				"Not Found", err.Error())
		case errors.Is(err, reporting.ErrInvalidQuery):
			ginmw.AbortWithProblem(c, http.StatusBadRequest, "validation_failed",
				"Validation Failed", err.Error())
		default:
			abortUnavailable(c)
		}
		return
	}
	c.JSON(http.StatusOK, reportPage{Rows: page.Rows, NextCursor: page.NextCursor})
}

// parseReportQuery parses and validates the request parameters; the report type
// is already shape-checked, merchant_id is not accepted (the source is the token).
func parseReportQuery(c *gin.Context, reportType string) (reporting.Query, error) {
	query := reporting.Query{ReportType: reportType, MerchantID: httpmw.MerchantID(c)}
	if raw := c.Query("period_from"); raw != "" {
		value, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return reporting.Query{}, errors.New("period_from must be RFC 3339")
		}
		query.PeriodFrom = &value
	}
	if raw := c.Query("period_to"); raw != "" {
		value, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return reporting.Query{}, errors.New("period_to must be RFC 3339")
		}
		query.PeriodTo = &value
	}
	if query.PeriodFrom != nil && query.PeriodTo != nil && query.PeriodTo.Before(*query.PeriodFrom) {
		return reporting.Query{}, errors.New("period_to must not be before period_from")
	}
	if raw := c.Query("group_by"); raw != "" {
		if len(raw) > maxGroupByLength {
			return reporting.Query{}, fmt.Errorf("group_by must be at most %d characters", maxGroupByLength)
		}
		query.GroupBy = raw
	}
	if raw := c.Query("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxReportLimit {
			return reporting.Query{}, fmt.Errorf("limit must be an integer between 1 and %d", maxReportLimit)
		}
		query.Limit = uint32(limit)
	} else {
		query.Limit = defaultReportLimit
	}
	if raw := c.Query("cursor"); raw != "" {
		if len(raw) > maxCursorLength {
			return reporting.Query{}, fmt.Errorf("cursor must be at most %d characters", maxCursorLength)
		}
		query.Cursor = raw
	}
	return query, nil
}

// abortUnknownReport responds 404 with the contract code unknown_report_type.
func abortUnknownReport(c *gin.Context) {
	ginmw.AbortWithProblem(c, http.StatusNotFound, "unknown_report_type",
		"Not Found", "тип отчета не найден")
}

// abortUnavailable responds 503 with a retry (reporting is unavailable).
func abortUnavailable(c *gin.Context) {
	c.Header("Retry-After", "1")
	ginmw.AbortWithProblem(c, http.StatusServiceUnavailable, "service_unavailable",
		"Service Unavailable", "отчетность временно недоступна")
}
