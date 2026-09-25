package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	_ "github.com/go-sql-driver/mysql"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/artifact"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/auth"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/config"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dispatch"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/files"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/handlers"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/mcp"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/mcpserver"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/notify"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/observer"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}

	resolver, err := files.NewResolver(cfg.FileRoots)
	if err != nil {
		slog.Error("file resolver init failed", "err", err)
		os.Exit(1)
	}
	for label, path := range resolver.Roots() {
		slog.Info("file root", "label", label, "path", path)
	}

	mcpClient := mcp.New(cfg.MCPURL, cfg.MCPBearerToken)
	dockerClient := dockerlog.New("/var/run/docker.sock")
	slog.Info("docker transport", "kind", dockerClient.Transport())

	var db *sql.DB
	if cfg.DBDSN != "" {
		db, err = sql.Open("mysql", cfg.DBDSN)
		if err != nil {
			slog.Error("db open failed (audit endpoints will 503)", "err", err)
		} else {
			db.SetMaxOpenConns(4)
			db.SetMaxIdleConns(2)
			db.SetConnMaxLifetime(5 * time.Minute)
		}
	} else {
		slog.Warn("DB_DSN empty — audit endpoints will 503")
	}

	var dbExec *sql.DB
	if cfg.DBExecDSN != "" {
		dbExec, err = sql.Open("mysql", cfg.DBExecDSN)
		if err != nil {
			slog.Error("db_exec open failed (db_exec MCP tool will refuse)", "err", err)
		} else {
			dbExec.SetMaxOpenConns(2)
			dbExec.SetMaxIdleConns(1)
			dbExec.SetConnMaxLifetime(5 * time.Minute)
		}
	} else {
		slog.Warn("DB_EXEC_DSN empty — db_exec MCP tool will refuse")
	}

	// Separate DSN for sql_import_* — needs multiStatements=true on the
	// driver. Falls back to DBExecDSN when unset; the import tool itself
	// surfaces a clear error if the resulting pool can't run multi-stmt SQL.
	importDB := dbExec
	if cfg.WowImportDSN != "" {
		if d, e := sql.Open("mysql", cfg.WowImportDSN); e != nil {
			slog.Error("wow import DSN open failed (sql_import_* will refuse)", "err", e)
		} else {
			d.SetMaxOpenConns(2)
			d.SetMaxIdleConns(1)
			d.SetConnMaxLifetime(5 * time.Minute)
			importDB = d
		}
	}

	// Realm tools live in acore_auth. Separate DSN so we don't have to
	// switch schemas on every call.
	var authDB *sql.DB
	if cfg.WowDBAuthDSN != "" {
		authDB, err = sql.Open("mysql", cfg.WowDBAuthDSN)
		if err != nil {
			slog.Error("auth DSN open failed (wow_check_realmlist / wow_update_realm_* will refuse)", "err", err)
		} else {
			authDB.SetMaxOpenConns(2)
			authDB.SetMaxIdleConns(1)
			authDB.SetConnMaxLifetime(5 * time.Minute)
		}
	} else {
		slog.Warn("OPS_DB_AUTH_DSN empty — wow_check_realmlist / wow_update_realm_* will refuse")
	}

	// Capture-delivery substrate: artifact store (hosted maps/screenshots/clips)
	// + out-of-band notifier (Telegram/Slack). Both degrade gracefully when
	// unconfigured — the store 503s its serve route, the notifier reports
	// pushed:[]. See docs/capture.md.
	artStore := &artifact.Store{Dir: cfg.ArtifactDir, BaseURL: cfg.ArtifactBaseURL}
	notifier := notify.New(notify.Options{
		TelegramBotToken: cfg.TelegramBotToken,
		TelegramChatID:   cfg.TelegramChatID,
		SlackBotToken:    cfg.SlackBotToken,
		SlackChannel:     cfg.SlackChannel,
	})
	// Observer-screenshot queue (Phase 2). Only wired when an observer character
	// is configured; otherwise the tool + endpoints refuse cleanly.
	var observerQueue *observer.Queue
	if cfg.ObserverChar != "" {
		observerQueue = observer.NewQueue(time.Duration(cfg.ObserverTTLSec) * time.Second)
	}
	slog.Info("capture substrate",
		"artifactStore", artStore.Enabled(),
		"notifyEnabled", notifier.Enabled(),
		"observer", cfg.ObserverChar != "")

	srv := handlers.NewServer(&handlers.Deps{
		Cfg:       cfg,
		MCP:       mcpClient,
		Docker:    dockerClient,
		Files:     resolver,
		DB:        db,
		DBExec:    dbExec,
		Artifacts: artStore,
		Notify:    notifier,
		Observer:  observerQueue,
	})

	// --- Admin MCP server (POST /mcp) ---
	mcpReg := mcpserver.NewRegistry()
	mcpserver.RegisterOpsTools(mcpReg, mcpserver.OpsDeps{Server: srv, Files: resolver})
	mcpserver.RegisterOpsAuditAdminTool(mcpReg, mcpserver.OpsDeps{Server: srv})
	mcpserver.RegisterOpsAuditAdminSummaryTool(mcpReg, mcpserver.OpsDeps{Server: srv})
	mcpserver.RegisterOpsAuditAdminLatencyProfileTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWorldserverHealthTool(mcpReg, mcpserver.OpsDeps{Server: srv})
	mcpserver.RegisterContainerTools(mcpReg, mcpserver.ContainerDeps{
		Docker:           dockerClient,
		DefaultContainer: cfg.AdminContainerDefault,
		DeniedKeyGlobs:   cfg.AdminDeniedKeys,
	})
	mcpserver.RegisterContainerStatsTool(mcpReg, mcpserver.ContainerDeps{
		Docker:           dockerClient,
		DefaultContainer: cfg.AdminContainerDefault,
	})
	mcpserver.RegisterContainerSystemInfoTool(mcpReg, mcpserver.ContainerDeps{
		Docker: dockerClient,
	})
	mcpserver.RegisterContainerEventsTool(mcpReg, mcpserver.ContainerDeps{
		Docker:           dockerClient,
		DefaultContainer: cfg.AdminContainerDefault,
	})
	mcpserver.RegisterWowEventsTimelineTool(mcpReg, mcpserver.ContainerDeps{
		Docker: dockerClient,
	})
	mcpserver.RegisterContainerLogsGrepTool(mcpReg, mcpserver.ContainerDeps{
		Docker:           dockerClient,
		DefaultContainer: cfg.AdminContainerDefault,
	})
	mcpserver.RegisterContainerLogsGrepContextTool(mcpReg, mcpserver.ContainerDeps{
		Docker:           dockerClient,
		DefaultContainer: cfg.AdminContainerDefault,
	})
	mcpserver.RegisterContainerLogsGrepContextWindowSummaryTool(mcpReg, mcpserver.ContainerDeps{
		Docker:           dockerClient,
		DefaultContainer: cfg.AdminContainerDefault,
	})
	mcpserver.RegisterContainerLogsGrepContextWindowSummaryTopNTool(mcpReg, mcpserver.ContainerDeps{
		Docker:           dockerClient,
		DefaultContainer: cfg.AdminContainerDefault,
	})
	mcpserver.RegisterOpsLogErrorsTopTool(mcpReg, mcpserver.ContainerDeps{
		Docker:           dockerClient,
		DefaultContainer: cfg.AdminContainerDefault,
	})
	mcpserver.RegisterOpsLogErrorsTopNormalizePreviewTool(mcpReg)
	mcpserver.RegisterOpsLogErrorsHistogramTool(mcpReg, mcpserver.ContainerDeps{
		Docker:           dockerClient,
		DefaultContainer: cfg.AdminContainerDefault,
	})
	mcpserver.RegisterOpsLogSeverityBreakdownTool(mcpReg, mcpserver.ContainerDeps{
		Docker:           dockerClient,
		DefaultContainer: cfg.AdminContainerDefault,
	})
	mcpserver.RegisterDBTools(mcpReg, mcpserver.DBDeps{QueryDB: db, ExecDB: dbExec})
	mcpserver.RegisterDBTableInfoTool(mcpReg, mcpserver.DBDeps{QueryDB: db, ExecDB: dbExec})
	mcpserver.RegisterDBExplainTool(mcpReg, mcpserver.DBDeps{QueryDB: db, ExecDB: dbExec})
	mcpserver.RegisterDBProcessListTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBGlobalStatusTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBGlobalStatusGrowthTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBInnodbStatusTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBConnectionSummaryTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBConnectionSummaryGrowthTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBSizeSummaryTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBTableBloatTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBIndexDataRatioTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBWidestCompositeIndexesTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBRedundantIndexesTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBUnindexedTablesTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBOverIndexedTablesTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBLowCardinalityIndexesTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBIndexStatsStalenessTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBAutoIncrementHeadroomTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterAhCrossHouseArbitrageTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBCharsetCollationAuditTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBEngineDistributionTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBRowFormatAuditTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBIndexKeyLengthAuditTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterDBColumnTypeDriftTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowPlayerLookupTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowGuildRosterTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowGuildSummaryTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowGuildMembershipChurnTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowGuildBankTabUtilizationTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowResetCharacterTool(mcpReg, mcpserver.DBDeps{QueryDB: db, ExecDB: dbExec})
	mcpserver.RegisterWowOnlinePlayersTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowBotsFleetStatusTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowBotLookupTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowBotLatencyProfileTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowGuildBankTopActorsTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowGuildMembershipActorsTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowBotSourceChannelBreakdownTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowBotTokenUsageTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowRecentLoginsTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowGuildBankSummaryTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowGuildBankItemBreakdownTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowMailSummaryTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowMailItemBreakdownTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowGuildBankItemFlowTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowMailOldestTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowMailUnreadTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowGuildBankActivityTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowAuthFailedAttemptsLogTool(mcpReg, mcpserver.ContainerDeps{
		Docker:           dockerClient,
		DefaultContainer: "ac-authserver",
	})
	mcpserver.RegisterWowFailedLoginsTopTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterWowGuildMembershipTargetsTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterAhMarketSummaryTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterAhBidderActivityTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterAhMarketTopSellersTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterAhUndercuttersTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterAhMarketTopItemsTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterAhMarketBidActivityTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterAhMarketCategoryBreakdownTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterAhItemContestRatioTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterAhItemPriceDispersionTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterAhMarketPricePercentilesTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterAhItemDemandTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterAhMarketPriceOutliersTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterAhMarketPriceOutliersBySellerTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterAhMarketExpiryTimelineTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterAhFloorSaturationTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterAhCrossHouseArbitrageDepthTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterAhSellerHouseFootprintTool(mcpReg, mcpserver.DBDeps{QueryDB: db})
	mcpserver.RegisterFileWriteTools(mcpReg, mcpserver.FileWriteDeps{
		Resolver:          resolver,
		ConfigRoot:        cfg.AdminConfigRoot,
		BackupDir:         cfg.AdminBackupDir,
		AllowedFiles:      cfg.AdminAllowedFiles,
		AllowedKeys:       cfg.AdminAllowedKeys,
		DeniedKeys:        cfg.AdminDeniedKeys,
		AllowedWriteGlobs: cfg.AdminWriteGlobs,
	})
	mcpserver.RegisterFileDiffTool(mcpReg, mcpserver.FileDiffDeps{
		ConfigRoot:   cfg.AdminConfigRoot,
		BackupDir:    cfg.AdminBackupDir,
		AllowedFiles: cfg.AdminAllowedFiles,
	})
	mcpserver.RegisterFileRestoreTool(mcpReg, mcpserver.FileRestoreDeps{
		ConfigRoot:   cfg.AdminConfigRoot,
		BackupDir:    cfg.AdminBackupDir,
		AllowedFiles: cfg.AdminAllowedFiles,
	})
	// master_wow.sh-equivalents: git pull (3 tools), sql import (2 tools),
	// wow_* admin (13 tools). All gated by AdminAllowActions + per-tool rate
	// limits; redact_args.go handles password redaction in the audit row.
	mcpserver.RegisterGitTools(mcpReg, mcpserver.GitDeps{
		WowRoot:       cfg.WowRoot,
		Runner:        mcpserver.DefaultGitRunner(),
		ModulePattern: cfg.WowModulePattern,
		// Same pool sql_import_dir uses — required when callers pass
		// import_sql:true on git_pull_module / git_pull_all.
		ExecDB: importDB,
	})
	mcpserver.RegisterSqlImportTools(mcpReg, mcpserver.SqlImportDeps{
		WowRoot: cfg.WowRoot,
		ExecDB:  importDB,
	})
	// Capture: render a synthetic tactical map from a get_scene_snapshot result,
	// host it, and push it to Telegram/Slack. See docs/capture.md.
	mcpserver.RegisterTacticalMapTool(mcpReg, mcpserver.TacMapDeps{
		Store:    artStore,
		Notifier: notifier,
	})
	// Capture: request REAL screenshots / video clips from the on-demand observer client.
	if observerQueue != nil {
		observerDeps := mcpserver.ObserverDeps{
			Queue:          observerQueue,
			MCP:            mcpClient,
			ObserverChar:   cfg.ObserverChar,
			WhisperBotGUID: cfg.ObserverWhisperBotGID,
			ClipTTL:        time.Duration(cfg.ObserverClipTTLSec) * time.Second,
			ClipMaxSec:     cfg.ObserverClipMaxSec,
		}
		mcpserver.RegisterObserverScreenshotTool(mcpReg, observerDeps)
		mcpserver.RegisterObserverClipTools(mcpReg, observerDeps)
	}
	mcpserver.RegisterWowTools(mcpReg, mcpserver.WowDeps{
		Docker:         dockerClient,
		BackupDir:      cfg.WowBackupDir,
		WowRoot:        cfg.WowRoot,
		DBContainer:    cfg.WowDBContainer,
		WorldContainer: cfg.WowWorldContainer,
		VolumeMounts:   cfg.WowVolumeMounts,
		VolumeNames:    cfg.WowVolumes,
		VolumeOwners:   cfg.WowVolumeOwners,
		AuthDB:         authDB,
	})
	// Audit row writes go through ops_rw (it has INSERT on the audit table). Falls
	// back to the read pool if ops_rw isn't configured — the table grant is
	// orthogonal to ops_ro's general SELECT-only stance, so the audit insert will
	// fail silently in that case (logged warning).
	auditDB := dbExec
	if auditDB == nil {
		auditDB = db
	}
	// Fold the flat tool list into facades before the server reads the
	// registry. Originals stay dispatchable by name; only advertisement
	// changes. OPS_MCP_FACADES=0 restores the flat list.
	facadeCount, foldedCount := 0, 0
	if cfg.AdminMcpFacades {
		facadeCount, foldedCount = mcpserver.InstallFacades(mcpReg)
	}

	mcpSrv := mcpserver.NewServer(mcpserver.ServerOptions{
		Registry:         mcpReg,
		BearerToken:      cfg.OpsBearerToken,
		AllowActions:     cfg.AdminAllowActions,
		AdminAuditDB:     auditDB,
		RateLimits:       cfg.AdminRateLimits,
		DefaultRateLimit: 30,
	})
	slog.Info("admin MCP registered",
		"tools", mcpReg.Len(),
		"advertised", len(mcpReg.AdvertisedSorted()),
		"facades", facadeCount,
		"folded", foldedCount,
		"allowActions", cfg.AdminAllowActions,
		"defaultContainer", cfg.AdminContainerDefault)

	r := chi.NewRouter()
	r.Use(chimiddleware.Recoverer)
	r.Use(requestLogger)

	r.Get("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	// MCP health is open (mirrors gameplay MCP at :18790/mcp/health). External
	// monitors/CI smoke checks need to verify liveness without a token.
	r.Get("/mcp/health", mcpSrv.HealthHandler())
	// CORS preflight for browser-based MCP clients. Bearer is the actual gate.
	r.Options("/mcp", mcpSrv.OptionsHandler())
	// Capture artifacts are served WITHOUT a bearer so the URL is browser-
	// clickable; the unguessable token in each filename + the host IP allowlist
	// are the gate. See handlers/artifacts.go and docs/capture.md.
	r.Get("/v1/artifacts/*", srv.ArtifactServe)

	r.Group(func(r chi.Router) {
		r.Use(auth.RequireBearer(cfg.OpsBearerToken))
		r.Get("/v1/status", srv.Status)
		r.Get("/v1/logs", srv.Logs)
		r.Get("/v1/logs/stream", srv.LogsStream)
		r.Get("/v1/files/list", srv.FilesList)
		r.Get("/v1/files/read", srv.FilesRead)
		r.Get("/v1/files/search", srv.FilesSearch)
		r.Get("/v1/audit/gateway", srv.AuditGateway)
		r.Get("/v1/audit/tactical", srv.AuditTactical)
		r.Get("/v1/audit/summary", srv.AuditSummary)
		r.Post("/v1/feedback", srv.FeedbackIngest)
		r.Get("/v1/feedback/results", srv.FeedbackResults)
		// Observer capture loop (the feedback-daemon polls + ships here).
		r.Get("/v1/observer/pending", srv.ObserverPending)
		r.Post("/v1/observer/screenshot", srv.ObserverScreenshot)
		r.Post("/v1/observer/clip", srv.ObserverClip)
	})
	// POST /mcp authenticates internally (constant-time), so it lives outside
	// the chi auth group to match the gameplay MCP's transport surface.
	r.Post("/mcp", mcpSrv.MCPHandler())

	httpServer := &http.Server{
		Addr:              cfg.BindAddr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: SSE follow streams are long-lived. The handler enforces its own deadline.
		IdleTimeout: 120 * time.Second,
	}

	go func() {
		slog.Info("listening", "addr", cfg.BindAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	// --- Feedback dispatch worker (PR 4 of feedback loop) ---
	dispatchCtx, cancelDispatch := context.WithCancel(context.Background())
	defer cancelDispatch()
	if cfg.FeedbackDispatchEnable {
		switch {
		case dbExec == nil:
			slog.Warn("dispatch worker requested but DB_EXEC_DSN empty; skipping")
		case cfg.FeedbackDispatchURL == "" || cfg.FeedbackDispatchBearer == "" || cfg.FeedbackDispatchModel == "":
			slog.Warn("dispatch worker requested but URL/BEARER/MODEL not all set; skipping")
		case cfg.FeedbackStorageDir == "":
			slog.Warn("dispatch worker requested but OPS_FEEDBACK_STORAGE_DIR empty; skipping")
		default:
			worker := dispatch.NewWorker(dispatch.Options{
				Store:         &dispatch.Store{DB: dbExec},
				Synthiq:       dispatch.New(cfg.FeedbackDispatchURL, cfg.FeedbackDispatchBearer, cfg.FeedbackDispatchModel),
				StorageDir:    cfg.FeedbackStorageDir,
				PollInterval:  time.Duration(cfg.FeedbackDispatchPollSec) * time.Second,
				StaleAfter:    time.Duration(cfg.FeedbackDispatchStaleSec) * time.Second,
				MaxImageBytes: int64(cfg.FeedbackDispatchMaxImageMB) * 1024 * 1024,
			})
			go worker.Run(dispatchCtx)
			slog.Info("feedback dispatch worker started",
				"poll_sec", cfg.FeedbackDispatchPollSec,
				"model", cfg.FeedbackDispatchModel)
		}
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	if db != nil {
		_ = db.Close()
	}
	if dbExec != nil {
		_ = dbExec.Close()
	}
	if authDB != nil {
		_ = authDB.Close()
	}
	// importDB is the same pool as dbExec when WowImportDSN is empty; only
	// close it when it's a distinct *sql.DB.
	if importDB != nil && importDB != dbExec {
		_ = importDB.Close()
	}
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := chimiddleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		// Skip noisy log spam for the SSE stream — it never returns until disconnect.
		if r.URL.Path == "/v1/logs/stream" && ww.Status() == http.StatusOK {
			return
		}
		slog.Info("req",
			"method", r.Method, "path", r.URL.Path,
			"status", ww.Status(), "bytes", ww.BytesWritten(),
			"dur_ms", time.Since(start).Milliseconds())
	})
}
