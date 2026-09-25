-- Audit log for gateway requests. Pruned by g_GatewayAuditRetentionDays.

CREATE TABLE IF NOT EXISTS mod_ollama_chat_gateway_audit (
    id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    bot_guid        BIGINT UNSIGNED NOT NULL,
    player_guid     BIGINT UNSIGNED NOT NULL,
    account_id      INT UNSIGNED NOT NULL,
    ts              DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    request_chars   INT UNSIGNED NOT NULL DEFAULT 0,
    response_chars  INT UNSIGNED NOT NULL DEFAULT 0,
    prompt_tokens   INT UNSIGNED NOT NULL DEFAULT 0,
    completion_tokens INT UNSIGNED NOT NULL DEFAULT 0,
    latency_ms      INT UNSIGNED NOT NULL DEFAULT 0,
    source_channel  VARCHAR(16) NOT NULL DEFAULT '',
    error           TINYINT(1) NOT NULL DEFAULT 0,
    INDEX idx_ts (ts),
    INDEX idx_bot (bot_guid),
    INDEX idx_account (account_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
