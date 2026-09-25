-- Player-facing controls for mod-ollama-chat:
--   * mod_ollama_chat_optouts: account-wide opt-out from any bot response (gateway or Ollama)
--   * mod_ollama_chat_bot_mutes: per-(account, bot) mute, finer-grained than opt-out

CREATE TABLE IF NOT EXISTS mod_ollama_chat_optouts (
    account_id  INT UNSIGNED NOT NULL PRIMARY KEY,
    opt_out_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE IF NOT EXISTS mod_ollama_chat_bot_mutes (
    account_id  INT UNSIGNED NOT NULL,
    bot_guid    BIGINT UNSIGNED NOT NULL,
    muted_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (account_id, bot_guid),
    INDEX idx_account (account_id),
    INDEX idx_bot (bot_guid)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
