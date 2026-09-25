-- Audit log for tactical (companion-mode) ticks. Pruned by g_TacticalAuditRetentionDays.

CREATE TABLE IF NOT EXISTS mod_ollama_chat_tactical_audit (
    id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    bot_guid        BIGINT UNSIGNED NOT NULL,
    bot_name        VARCHAR(32) NOT NULL DEFAULT '',
    ts              DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    action          VARCHAR(64) NOT NULL DEFAULT '',
    result          VARCHAR(16) NOT NULL DEFAULT '',  -- 'ok' | 'error' | 'blocked'
    error           VARCHAR(255) NULL,
    escalated       TINYINT(1) NOT NULL DEFAULT 0,
    escalation_reason VARCHAR(255) NULL,
    latency_ms      INT UNSIGNED NOT NULL DEFAULT 0,
    in_combat       TINYINT(1) NOT NULL DEFAULT 0,
    INDEX idx_ts (ts),
    INDEX idx_bot_ts (bot_guid, ts),
    INDEX idx_action (action)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
