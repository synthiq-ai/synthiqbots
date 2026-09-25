-- jev decision tier: record which backend produced each audit row.
--
-- NOTE: this file is DOCUMENTATION for fresh installs. The live worldserver
-- image bakes AC_UPDATES_ENABLE_DATABASES=0, so nothing under updates/ is
-- applied automatically. The module self-migrates at startup instead
-- (Jev::EnsureAuditBackendColumns in src/mod-ollama-chat_jev.cpp): it checks
-- INFORMATION_SCHEMA.COLUMNS and runs exactly these ALTERs when a column is
-- missing. Keep the two in lockstep.
--
-- backend: 'jev' | 'llm' | 'det' | NULL (rows written before the migration,
--          or state-transition rows with no model behind them).

ALTER TABLE mod_ollama_chat_gateway_audit
    ADD COLUMN backend VARCHAR(8) NULL;

ALTER TABLE mod_ollama_chat_tactical_audit
    ADD COLUMN backend VARCHAR(8) NULL,
    ADD COLUMN prompt_tokens INT UNSIGNED NOT NULL DEFAULT 0;
