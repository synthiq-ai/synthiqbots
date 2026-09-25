package handlers

import (
	"database/sql"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/artifact"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/config"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/files"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/mcp"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/notify"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/observer"
)

// Deps is the dependency bundle handed to every handler. Built once in main.
type Deps struct {
	Cfg       *config.Config
	MCP       *mcp.Client
	Docker    *dockerlog.Client
	Files     *files.Resolver
	DB        *sql.DB          // ops_ro: may be nil if DB_DSN was empty (audit endpoints will 503)
	DBExec    *sql.DB          // ops_rw: may be nil if DB_EXEC_DSN was empty (db_exec MCP tool will refuse)
	Artifacts *artifact.Store  // capture-artifact store; nil/disabled => artifact serve 503s
	Notify    *notify.Notifier // out-of-band push (Telegram/Slack); nil-safe
	Observer  *observer.Queue  // pending observer-screenshot captures; nil/disabled => observer endpoints 503
}
