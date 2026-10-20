// main starts the billing-api HTTP server of the CBL platform.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/ZMichael3012/cbl-kit/ginmw"
	"github.com/ZMichael3012/cbl-kit/storagex"

	"github.com/cbl/cbl-billing-api/internal/connector"
	"github.com/cbl/cbl-billing-api/internal/ledger"
	"github.com/cbl/cbl-billing-api/internal/payment"
	"github.com/cbl/cbl-billing-api/internal/reporting"
	"github.com/cbl/cbl-billing-api/internal/storage"
	"github.com/cbl/cbl-billing-api/internal/transport"
	"github.com/cbl/cbl-billing-api/internal/transport/httpmw"
	"github.com/cbl/cbl-billing-api/internal/usecase"
)

// Default configuration values.
const (
	defaultCacheTTL         = 5 * time.Minute
	defaultDenyTTL          = time.Minute
	defaultGrace            = 24 * time.Hour
	defaultIdempotencyTTL   = 24 * time.Hour
	defaultProcessingExpiry = 2 * time.Minute
	defaultReportingTimeout = 5 * time.Second
	defaultDomainTimeout    = 5 * time.Second
)

// main starts the service and exits the process with a non-zero code on an
// initialization error (the orchestrator must see a failure, not a successful
// exit).
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

// run assembles the dependencies and serves requests until a shutdown signal.
func run() error {
	log, err := zap.NewProduction()
	if err != nil {
		return fmt.Errorf("logger: %w", err)
	}
	defer func() { _ = log.Sync() }()

	dsn := os.Getenv("PG_DSN")
	if dsn == "" {
		return errors.New("PG_DSN is required")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("resolve sql db: %w", err)
	}
	maxOpen := min(intEnv("DB_MAX_OPEN_CONNS", 20), 100)
	maxIdle := min(intEnv("DB_MAX_IDLE_CONNS", 10), maxOpen)
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)

	httpAddr := getenv("HTTP_ADDR", ":8010")
	metricsAddr := getenv("METRICS_ADDR", ":9210")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	migrationsDir := getenv("MIGRATIONS_DIR", "./migrations")
	if err := storagex.Migrate(ctx, db, migrationsDir); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	repo := storage.NewRepository(db)

	// The Redis cache is optional: without it key verification works via
	// PostgreSQL (fail-safe for security, latency degradation).
	var cache usecase.KeyCache
	if addr := os.Getenv("REDIS_ADDR"); addr != "" {
		redisCache, err := storage.NewRedisCache(addr)
		if err != nil {
			return fmt.Errorf("redis cache: %w", err)
		}
		defer func() { _ = redisCache.Close() }()
		if pingErr := redisCache.Ping(ctx); pingErr != nil {
			log.Warn("redis is not reachable, api key verification uses postgres", zap.Error(pingErr))
		}
		cache = redisCache
	} else {
		log.Warn("REDIS_ADDR is empty: api key verification uses postgres only")
	}

	grace, err := durationEnv("API_KEY_ROTATION_GRACE", defaultGrace)
	if err != nil {
		return err
	}
	cacheTTL, err := durationEnv("API_KEY_CACHE_TTL", defaultCacheTTL)
	if err != nil {
		return err
	}
	denyTTL, err := durationEnv("API_KEY_DENY_CACHE_TTL", defaultDenyTTL)
	if err != nil {
		return err
	}
	keys, err := usecase.NewAPIKeys(repo, cache, log, usecase.KeysConfig{
		CacheTTL:      cacheTTL,
		DenyTTL:       denyTTL,
		RotationGrace: grace,
	})
	if err != nil {
		return fmt.Errorf("api keys configuration: %w", err)
	}
	log.Info("api keys configured",
		zap.Duration("rotation_grace", grace),
		zap.Duration("cache_ttl", cacheTTL),
		zap.Duration("deny_cache_ttl", denyTTL),
		zap.Bool("redis_cache", cache != nil))

	idempotencyTTL, err := durationEnv("IDEMPOTENCY_TTL", defaultIdempotencyTTL)
	if err != nil {
		return err
	}
	processingTimeout, err := durationEnv("IDEMPOTENCY_PROCESSING_TIMEOUT", defaultProcessingExpiry)
	if err != nil {
		return err
	}
	idempotency, err := usecase.NewIdempotency(repo, cache, log, usecase.IdempotencyConfig{
		TTL:               idempotencyTTL,
		ProcessingTimeout: processingTimeout,
	})
	if err != nil {
		return fmt.Errorf("idempotency configuration: %w", err)
	}
	log.Info("idempotency configured",
		zap.Duration("ttl", idempotencyTTL),
		zap.Duration("processing_timeout", processingTimeout))

	// Reporting (11.4): the Reporting address is optional - without it the
	// /v1/reports proxy responds 503 while the rest of the merchant API works.
	// The timeout is read and validated unconditionally (bounds are below the
	// server WriteTimeout).
	var reportsClient transport.ReportsClient
	reportingTimeout, err := durationEnv("REPORTING_TIMEOUT", defaultReportingTimeout)
	if err != nil {
		return err
	}
	if reportingTimeout < time.Second || reportingTimeout > 10*time.Second {
		return fmt.Errorf("REPORTING_TIMEOUT is out of range: %s", reportingTimeout)
	}
	reportingAddr := os.Getenv("REPORTING_GRPC_ADDR")
	if reportingAddr != "" {
		client, err := reporting.NewClient(reportingAddr, reportingTimeout)
		if err != nil {
			return fmt.Errorf("reporting client: %w", err)
		}
		defer func() { _ = client.Close() }()
		reportsClient = client
		log.Info("reports proxy configured",
			zap.String("addr", reportingAddr), zap.Duration("timeout", reportingTimeout))
	} else {
		log.Warn("REPORTING_GRPC_ADDR is empty: /v1/reports responds 503")
	}

	router := gin.New()
	router.Use(ginmw.RequestID(), ginmw.AccessLog(log), ginmw.Recovery(log))
	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.GET("/readyz", func(c *gin.Context) {
		pingCtx, cancel := context.WithTimeout(c.Request.Context(), time.Second)
		defer cancel()
		if err := repo.Ping(pingCtx); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Merchant API /v1: key authentication and POST idempotency; the domain
	// routes of 7.4 depend on Payment, Ledger and the Connector capabilities.
	merchantAPI := router.Group("/v1",
		httpmw.Require(func(ctx context.Context, token string) (string, error) {
			result, err := keys.Verify(ctx, token)
			return result.MerchantID, err
		}),
		httpmw.Idempotency(idempotency),
	)

	// The domain clients are optional at startup: without an address the
	// affected routes answer 503 service_unavailable while the rest of the
	// merchant API keeps working (T-BA-12, FR-7).
	var paymentsClient usecase.PaymentClient
	paymentTimeout, err := domainTimeout("PAYMENT_TIMEOUT")
	if err != nil {
		return err
	}
	if addr := os.Getenv("PAYMENT_GRPC_ADDR"); addr != "" {
		client, err := payment.NewClient(addr, paymentTimeout)
		if err != nil {
			return fmt.Errorf("payment client: %w", err)
		}
		defer func() { _ = client.Close() }()
		paymentsClient = client
		log.Info("payment client configured", zap.String("addr", addr), zap.Duration("timeout", paymentTimeout))
	} else {
		log.Warn("PAYMENT_GRPC_ADDR is empty: invoice routes respond 503")
	}

	var ledgerClient usecase.LedgerClient
	ledgerTimeout, err := domainTimeout("LEDGER_TIMEOUT")
	if err != nil {
		return err
	}
	if addr := os.Getenv("LEDGER_GRPC_ADDR"); addr != "" {
		client, err := ledger.NewClient(addr, ledgerTimeout)
		if err != nil {
			return fmt.Errorf("ledger client: %w", err)
		}
		defer func() { _ = client.Close() }()
		ledgerClient = client
		log.Info("ledger client configured", zap.String("addr", addr), zap.Duration("timeout", ledgerTimeout))
	} else {
		log.Warn("LEDGER_GRPC_ADDR is empty: balances and statement routes respond 503")
	}

	var capabilitiesClient transport.CapabilitiesClient
	connectorTimeout, err := domainTimeout("CONNECTOR_TIMEOUT")
	if err != nil {
		return err
	}
	if addr := os.Getenv("CONNECTOR_GRPC_ADDR"); addr != "" {
		client, err := connector.NewClient(addr, connectorTimeout)
		if err != nil {
			return fmt.Errorf("connector client: %w", err)
		}
		defer func() { _ = client.Close() }()
		capabilitiesClient = client
		log.Info("connector capabilities configured", zap.String("addr", addr), zap.Duration("timeout", connectorTimeout))
	} else {
		log.Warn("CONNECTOR_GRPC_ADDR is empty: invoice creation responds 503")
	}

	var invoiceService *usecase.InvoiceService
	if paymentsClient != nil {
		invoiceService = usecase.NewInvoiceService(paymentsClient, repo, log)
	}
	var statementService *usecase.StatementService
	if ledgerClient != nil {
		statementService = usecase.NewStatementService(ledgerClient, paymentsClient, log, nil)
	}

	transport.RegisterInvoices(merchantAPI, transport.InvoicesDeps{
		Invoices:     invoiceService,
		Capabilities: capabilitiesClient,
		Log:          log,
	})
	transport.RegisterStatements(merchantAPI, transport.StatementDeps{
		Statements: statementService,
		Log:        log,
	})
	transport.RegisterReports(merchantAPI, reportsClient)

	httpSrv := &http.Server{
		Addr:              httpAddr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	metricsSrv := &http.Server{Addr: metricsAddr, Handler: metricsHandler(), ReadHeaderTimeout: 5 * time.Second}

	go func() {
		log.Info("http server started", zap.String("addr", httpAddr))
		if serveErr := httpSrv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Error("http server failed", zap.Error(serveErr))
			stop()
		}
	}()
	go func() {
		log.Info("metrics server started", zap.String("addr", metricsAddr))
		if serveErr := metricsSrv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Error("metrics server failed", zap.Error(serveErr))
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutdown started")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if shutdownErr := httpSrv.Shutdown(shutdownCtx); shutdownErr != nil {
		log.Error("http shutdown failed", zap.Error(shutdownErr))
	}
	if shutdownErr := metricsSrv.Shutdown(shutdownCtx); shutdownErr != nil {
		log.Error("metrics shutdown failed", zap.Error(shutdownErr))
	}
	log.Info("shutdown finished")
	return nil
}

// metricsHandler serves the metrics stub until task 0.7.
func metricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprintln(w, "# metrics endpoint placeholder (task 0.7)")
	})
	return mux
}

// getenv returns the environment variable or the fallback value.
func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// durationEnv parses a duration from the environment; an invalid value is an
// error (fail-safe against a silent typo in the configuration).
func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return parsed, nil
}

// intEnv parses an integer from the environment; an invalid value returns the
// fallback (the DB pool does not affect security).
func intEnv(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

// domainTimeout reads and validates a domain client timeout: the default is
// 5 s, the bounds are 1 s to 10 s (below the server WriteTimeout).
func domainTimeout(name string) (time.Duration, error) {
	timeout, err := durationEnv(name, defaultDomainTimeout)
	if err != nil {
		return 0, err
	}
	if timeout < time.Second || timeout > 10*time.Second {
		return 0, fmt.Errorf("%s is out of range: %s", name, timeout)
	}
	return timeout, nil
}
