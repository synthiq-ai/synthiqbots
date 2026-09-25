-- Audit log for the admin MCP (POST /mcp on wow-ops-api). One row per
-- tools/call dispatch — both successful and failed. Allows per-IP / per-tool
-- forensics for db_exec, container_*, file_set_key, file_write.

CREATE TABLE IF NOT EXISTS mod_ollama_chat_admin_audit (
    id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    ts          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    client_ip   VARCHAR(64) NOT NULL DEFAULT '',
    tool        VARCHAR(64) NOT NULL DEFAULT '',
    args_json   TEXT NULL,
    result      ENUM('ok','error') NOT NULL DEFAULT 'ok',
    error       VARCHAR(512) NULL,
    duration_ms INT UNSIGNED NOT NULL DEFAULT 0,
    INDEX idx_ts (ts),
    INDEX idx_tool (tool)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
