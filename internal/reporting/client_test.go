package reporting

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cbl/cbl-billing-api/test/reportingstub"
)

// startStub поднимает тестовый ReportingService и клиент к нему.
func startStub(t *testing.T) (*reportingstub.Server, *Client) {
	t.Helper()
	stub := reportingstub.New()
	addr, err := stub.Start("")
	require.NoError(t, err)
	t.Cleanup(stub.Stop)
	client, err := NewClient(addr, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return stub, client
}

// TestQueryPage проверяет успешную страницу и передачу параметров.
func TestQueryPage(t *testing.T) {
	stub, client := startStub(t)
	page, err := client.Query(t.Context(), Query{
		ReportType: "payments", MerchantID: "m-1", Limit: 5, GroupBy: "asset",
	})
	require.NoError(t, err)
	require.Len(t, page.Rows, 1)
	assert.Equal(t, "m-1", page.Rows[0]["merchant_id"])
	assert.Equal(t, "5", page.Rows[0]["limit"])
	assert.Equal(t, "asset", page.Rows[0]["group_by"])
	assert.Empty(t, page.NextCursor)

	requests := stub.Requests()
	require.Len(t, requests, 1)
	assert.Equal(t, "m-1", requests[0].GetMerchantId())
	assert.Equal(t, uint32(5), requests[0].GetLimit())
}

// TestQueryPeriods проверяет передачу границ периода.
func TestQueryPeriods(t *testing.T) {
	stub, client := startStub(t)
	from := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	_, err := client.Query(t.Context(), Query{
		ReportType: "payments", MerchantID: "m-1", PeriodFrom: &from, PeriodTo: &to,
	})
	require.NoError(t, err)
	requests := stub.Requests()
	require.Len(t, requests, 1)
	require.NotNil(t, requests[0].GetPeriodFrom())
	assert.Equal(t, from.UnixMilli(), requests[0].GetPeriodFrom().AsTime().UnixMilli())
	assert.Equal(t, to.UnixMilli(), requests[0].GetPeriodTo().AsTime().UnixMilli())
}

// TestQueryErrorClassification проверяет классификацию статусов gRPC.
func TestQueryErrorClassification(t *testing.T) {
	_, client := startStub(t)

	_, err := client.Query(t.Context(), Query{ReportType: "invoices"})
	require.ErrorIs(t, err, ErrUnknownReportType)

	_, err = client.Query(t.Context(), Query{ReportType: "violate"})
	require.ErrorIs(t, err, ErrInvalidQuery)

	_, err = client.Query(t.Context(), Query{ReportType: "down"})
	require.ErrorIs(t, err, ErrUnavailable)
}

// TestQueryUnavailable проверяет классификацию обрыва соединения.
func TestQueryUnavailable(t *testing.T) {
	stub, client := startStub(t)
	stub.Stop()
	_, err := client.Query(t.Context(), Query{ReportType: "payments", MerchantID: "m-1"})
	require.ErrorIs(t, err, ErrUnavailable)
}

// TestNewClientValidation проверяет валидацию адреса и таймаута.
func TestNewClientValidation(t *testing.T) {
	_, err := NewClient("no-port", time.Second)
	require.Error(t, err)
	_, err = NewClient("127.0.0.1:9000", 0)
	require.Error(t, err)
}

// TestClassifyNonStatus проверяет отнесение нестатусных ошибок к недоступности.
func TestClassifyNonStatus(t *testing.T) {
	require.ErrorIs(t, classify(errors.New("transport closed")), ErrUnavailable)
}
