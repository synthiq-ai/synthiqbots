# Deployment

This public mirror does not ship the operator's deployment pipeline. The production
CI/CD (self-hosted runner, worldserver rebuild and restart, uptime canary, e2e suite)
and the host runbooks live in a private repository.

To deploy the module yourself:

1. Place this repository at `modules/mod-ollama-chat` inside your AzerothCore
   (mod-playerbots fork) source tree — see [BUILD.md](BUILD.md).
2. Build the worldserver as usual (CMake, or the AzerothCore Docker build with
   `docker/Dockerfile.ollama`).
3. Copy `conf/mod_ollama_chat.conf.dist` to your server's `etc/modules/mod_ollama_chat.conf`
   and set your own endpoints and credentials. Per-bot gateway routing goes in a
   `gateway_overrides.json` file (`OllamaChat.Gateway.BotOverridesJsonFile`).
4. Optional side services: `apps/ops-api` (admin MCP) and `apps/feedback-daemon`
   ship their own Dockerfiles and READMEs.

Hostnames and IPs that appear in the docs (`*.example.com`, `192.168.100.x`,
`203.0.113.x`) are placeholders.
