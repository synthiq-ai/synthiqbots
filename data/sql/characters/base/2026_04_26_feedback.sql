-- In-game feedback queue (addon → ops-api ingest → dispatch worker).
-- One row per /sb feedback capture. Server-side wow-ops-api inserts on
-- POST /v1/feedback; the dispatch worker (PR 4) updates status +
-- agent_response + agent_pr_url after it routes to the gateway agent.
--
-- Manual one-time grant on game-host (NOT auto-applied by AzerothCore):
--   GRANT SELECT, INSERT, UPDATE ON acore_characters.mod_ollama_chat_feedback
--     TO 'ops_rw'@'%';
--   GRANT SELECT ON acore_characters.mod_ollama_chat_feedback
--     TO 'ops_ro'@'%';
-- (ops_ro already has SELECT on mod_ollama_chat_% via the wildcard grant.)

CREATE TABLE IF NOT EXISTS mod_ollama_chat_feedback (
    id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    addon_id        VARCHAR(64) NOT NULL DEFAULT '',
    ts              DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    addon_ts        BIGINT UNSIGNED NOT NULL DEFAULT 0,
    char_name       VARCHAR(64) NOT NULL DEFAULT '',
    char_realm      VARCHAR(64) NOT NULL DEFAULT '',
    char_class      VARCHAR(32) NOT NULL DEFAULT '',
    char_lvl        SMALLINT UNSIGNED NOT NULL DEFAULT 0,
    zone            VARCHAR(64) NOT NULL DEFAULT '',
    note            TEXT NULL,
    chat_history    MEDIUMTEXT NULL,
    ctx_json        MEDIUMTEXT NULL,
    image_path      VARCHAR(255) NOT NULL DEFAULT '',
    image_bytes     INT UNSIGNED NOT NULL DEFAULT 0,
    image_mime      VARCHAR(64) NOT NULL DEFAULT '',
    status          ENUM('pending','dispatched','resolved','error') NOT NULL DEFAULT 'pending',
    agent_response  MEDIUMTEXT NULL,
    agent_actions   TEXT NULL,
    agent_pr_url    VARCHAR(255) NULL,
    dispatched_at   DATETIME NULL,
    resolved_at     DATETIME NULL,
    INDEX idx_ts (ts),
    INDEX idx_status (status),
    INDEX idx_addon_id (addon_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
