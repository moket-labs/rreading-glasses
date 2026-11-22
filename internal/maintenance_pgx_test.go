package internal

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

func maintenanceDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("MAINTENANCE_TEST_DSN")
	if dsn == "" {
		// Same disposable database as the repository's native CI tests.
		dsn = "postgres://postgres@localhost:5432/test"
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	require.Equal(t, "test", cfg.ConnConfig.Database)
	require.Equal(t, "postgres", cfg.ConnConfig.User)
	require.True(t, cfg.ConnConfig.Host == "localhost" || strings.HasPrefix(cfg.ConnConfig.Host, "/"), "only loopback or local Unix test DB")
	return dsn
}

func maintenanceMetric(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err, "pedantic gather")
	for _, family := range families {
		if family.GetName() == name {
			require.Len(t, family.Metric, 1)
			switch family.GetType() {
			case dto.MetricType_COUNTER:
				return family.Metric[0].GetCounter().GetValue()
			case dto.MetricType_GAUGE:
				return family.Metric[0].GetGauge().GetValue()
			default:
				t.Fatalf("unexpected metric type %s", family.GetType())
			}
		}
	}
	t.Fatalf("required metric %s absent", name)
	return 0
}

// Exercise the production registration with a real exhausted PostgreSQL pool.
func TestMaintenancePGXPedanticWaitMetric(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	db, err := newDB(ctx, maintenanceDSN(t))
	require.NoError(t, err)
	defer db.Close()
	// Seed before production db-stat goroutine (SUM on an empty table is NULL).
	_, err = db.Exec(ctx, "INSERT INTO cache(key,value) VALUES('maintenance-metrics', $1) ON CONFLICT(key) DO NOTHING", []byte{1})
	require.NoError(t, err)
	cfg := db.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	defer pool.Close()
	reg := prometheus.NewPedanticRegistry()
	newDBMetrics(pool, reg)
	require.Eventually(t, func() bool {
		fs, err := reg.Gather()
		if err != nil {
			return false
		}
		for _, f := range fs {
			if f.GetName() == "rg_db_total" && len(f.Metric) == 7 {
				return true
			}
		}
		return false
	}, 3*time.Second, time.Millisecond)
	held, err := pool.Acquire(ctx)
	require.NoError(t, err)
	before := pool.Stat().EmptyAcquireCount()
	started := make(chan struct{})
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		close(started)
		conn, err := pool.Acquire(ctx)
		if err == nil {
			conn.Release()
		}
		done <- err
	}()
	<-started
	time.Sleep(40 * time.Millisecond)
	select {
	case err := <-done:
		held.Release()
		t.Fatalf("pool acquisition did not wait: %v", err)
	default:
	}
	held.Release()
	require.NoError(t, <-done)
	elapsed := time.Since(start)
	require.Greater(t, pool.Stat().EmptyAcquireCount(), before)
	value := maintenanceMetric(t, reg, "pgxpool_empty_acquire_wait_time_ns")
	require.GreaterOrEqual(t, value, float64((30 * time.Millisecond).Nanoseconds()), "metric must be raw ns, not seconds")
	require.LessOrEqual(t, value, float64((elapsed + time.Second).Nanoseconds()))
	require.Equal(t, float64(pool.Stat().EmptyAcquireCount()), maintenanceMetric(t, reg, "pgxpool_empty_acquire"))
	recorder := httptest.NewRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{ErrorHandling: promhttp.HTTPErrorOnError}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/debug/metrics", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), "pgxpool_empty_acquire_wait_time_ns ")
	require.NotContains(t, recorder.Body.String(), "pgxpool_empty_acquire_wait_time_seconds")
	// Scrape the actual production mux + instrumentation route too.
	mux := NewMux(&Handler{}, reg)
	production := httptest.NewRecorder()
	mux.ServeHTTP(production, httptest.NewRequest(http.MethodGet, "/debug/metrics", nil))
	require.Equal(t, http.StatusOK, production.Code)
	require.Contains(t, production.Body.String(), "pgxpool_empty_acquire_wait_time_ns ")
	_, err = reg.Gather()
	require.NoError(t, err)
	t.Logf("pedantic production collector scrape OK: wait_ns=%v elapsed=%s empty_acquires=%d", value, elapsed, pool.Stat().EmptyAcquireCount())
}

func TestMaintenancePGXCachePersisterColdBoundary(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	dsn := maintenanceDSN(t)
	warm, err := NewCache(ctx, dsn, nil, nil)
	require.NoError(t, err)
	pg := warm.wrapped[1].(*pgcache)
	defer pg.db.Close()
	key := "maintenance-byte-boundary"
	payload := append([]byte("雪🙂\x00'; DROP TABLE cache; --\n"), bytes.Repeat([]byte{0, 1, 127, 128, 255}, 8192)...)
	warm.Set(ctx, key, payload, time.Hour)
	var stored []byte
	require.NoError(t, pg.db.QueryRow(ctx, "SELECT value FROM cache WHERE key=$1", key).Scan(&stored))
	require.True(t, bytes.HasPrefix(stored, []byte{0x1f, 0x8b}), "real stored gzip")
	require.NotEqual(t, payload, stored)
	cold, err := NewCache(ctx, dsn, nil, nil)
	require.NoError(t, err)
	defer cold.wrapped[1].(*pgcache).db.Close()
	got, ttl, ok := cold.GetWithTTL(ctx, key)
	require.True(t, ok)
	require.Equal(t, payload, got)
	require.Greater(t, ttl, 50*time.Minute)
	require.NoError(t, cold.Expire(ctx, key))
	got, ttl, ok = cold.GetWithTTL(ctx, key)
	require.True(t, ok)
	require.Zero(t, ttl)
	require.Equal(t, payload, got)
	p, err := NewPersister(ctx, cold, dsn)
	require.NoError(t, err)
	defer p.db.Close()
	ids := []int64{9007199254740993, 42, 9223372036854775806}
	for _, id := range ids {
		require.NoError(t, p.Persist(ctx, id, payload))
		time.Sleep(2 * time.Millisecond)
	}
	reopened, err := NewPersister(ctx, warm, dsn)
	require.NoError(t, err)
	defer reopened.db.Close()
	recovered, err := reopened.Persisted(ctx)
	require.NoError(t, err)
	require.Equal(t, ids, recovered, "real pgtype timestamptz + integer recovery")
	for _, id := range ids {
		require.NoError(t, reopened.Delete(ctx, id))
	}
	require.NoError(t, cold.Delete(ctx, key))
	require.NoError(t, warm.Delete(ctx, key))
	got, ok = warm.Get(ctx, key)
	require.False(t, ok)
	require.Nil(t, got)
	t.Logf("real cold cache gzip/bytea/TTL + reopened persister recovered exact %v", ids)
}

func TestMaintenancePGXClientCancellationAndParameters(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	db, err := newDB(ctx, maintenanceDSN(t))
	require.NoError(t, err)
	defer db.Close()
	for _, mode := range []pgx.QueryExecMode{pgx.QueryExecModeCacheStatement, pgx.QueryExecModeSimpleProtocol} {
		literal := "雪🙂'\\'; SELECT pg_sleep(99); --\n"
		var got string
		var number int64
		require.NoError(t, db.QueryRow(ctx, "SELECT $1::text, $2::bigint", mode, literal, int64(9007199254740993)).Scan(&got, &number))
		require.Equal(t, literal, got)
		require.Equal(t, int64(9007199254740993), number)
	}
	tx, err := db.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, "INSERT INTO cache(key,value) VALUES($1,$2)", "maintenance-rollback", []byte{9})
	require.NoError(t, err)
	require.NoError(t, tx.Rollback(ctx))
	var n int
	require.NoError(t, db.QueryRow(ctx, "SELECT count(*) FROM cache WHERE key=$1", "maintenance-rollback").Scan(&n))
	require.Zero(t, n)
	short, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	_, err = db.Exec(short, "SELECT pg_sleep(5)")
	stop()
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, db.Ping(ctx), "pool must recover after canceled real query")
	batch := &pgx.Batch{}
	batch.Queue("SELECT $1::text", strings.Repeat("雪", 100))
	batch.Queue("SELECT $1::bigint", int64(9223372036854775806))
	results := db.SendBatch(ctx, batch)
	var text string
	var id int64
	require.NoError(t, results.QueryRow().Scan(&text))
	require.NoError(t, results.QueryRow().Scan(&id))
	require.NoError(t, results.Close())
	require.Equal(t, strings.Repeat("雪", 100), text)
	require.Equal(t, int64(9223372036854775806), id)
	t.Logf("prepared/simple parameters, rollback, query cancellation/recovery, batch OK: %d", id)
}
