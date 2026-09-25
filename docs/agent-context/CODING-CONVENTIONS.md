<!-- Extracted from CLAUDE.md by claude-md-refactor | Last verified: 2026-05-04 -->

# Coding Conventions

- AzerothCore's ScriptMgr pattern: register scripts via `new ClassName()` in `Addmod_ollama_chatScripts()`
- Uses AzerothCore's `LOG_INFO`/`LOG_DEBUG`/`LOG_ERROR` macros for logging
- `fmt::format` for string formatting (not `printf` or `std::format`)
- Config keys follow `OllamaChat.SectionName` pattern
- Chat channel types use the `ChatChannelSourceLocal` enum for internal routing
