package repository

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

const oneHourWindowPattern = `(?s)checked_at >= NOW\(\) - INTERVAL '1 hour'`

func TestComputeAvailabilityUsesOneHourWindow(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	repo := &channelMonitorRepository{db: db}
	mock.ExpectQuery(oneHourWindowPattern).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"model", "total", "ok", "avg_latency_ms"}).
			AddRow("gpt-test", 60, 59, 125.4))

	rows, err := repo.ComputeAvailability(context.Background(), 42)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "gpt-test", rows[0].Model)
	require.InDelta(t, 98.3333, rows[0].AvailabilityPct, 0.0001)
	require.NotNil(t, rows[0].AvgLatencyMs)
	require.Equal(t, 125, *rows[0].AvgLatencyMs)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestComputeAvailabilityForMonitorsUsesOneHourWindow(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	repo := &channelMonitorRepository{db: db}
	mock.ExpectQuery(oneHourWindowPattern).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"monitor_id", "model", "total", "ok", "avg_latency_ms"}).
			AddRow(int64(42), "gpt-test", 60, 60, 100.0))

	rows, err := repo.ComputeAvailabilityForMonitors(context.Background(), []int64{42})
	require.NoError(t, err)
	require.Len(t, rows[42], 1)
	require.Equal(t, 100.0, rows[42][0].AvailabilityPct)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestListRecentHistoryForMonitorsUsesOneHourWindow(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	repo := &channelMonitorRepository{db: db}
	mock.ExpectQuery(oneHourWindowPattern).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), 60).
		WillReturnRows(sqlmock.NewRows([]string{"monitor_id", "status", "latency_ms", "ping_latency_ms", "checked_at"}))

	rows, err := repo.ListRecentHistoryForMonitors(
		context.Background(),
		[]int64{42},
		map[int64]string{42: "gpt-test"},
		60,
	)
	require.NoError(t, err)
	require.Empty(t, rows[42])
	require.NoError(t, mock.ExpectationsWereMet())
}
