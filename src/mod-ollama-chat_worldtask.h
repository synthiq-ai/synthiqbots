#ifndef MOD_OLLAMA_CHAT_WORLDTASK_H
#define MOD_OLLAMA_CHAT_WORLDTASK_H

#include <nlohmann/json.hpp>

#include <cstdint>
#include <functional>

// (2026-09-24) Run a closure on the WORLD thread and wait for its result.
//
// MCP tool handlers run on the MCP listener thread and the gateway prompt is
// built on a detached worker; neither may mutate groups, playerbots masters or
// read the character cache safely. Post the work here instead: it runs at the
// next world update (OllamaChatConfigWorldScript::OnUpdate -> Drain), and the
// caller waits up to timeoutMs. A timed-out caller gets {"error": ...}; the
// task still runs later and its result is dropped (shared state, no dangling
// pointers). Pass GUIDs into the closure and resolve Player* inside it.
namespace OllamaChat::WorldTask
{
    nlohmann::json Run(std::function<nlohmann::json()> fn, uint32_t timeoutMs = 3000);

    // World thread only: run every queued task. Called from OnUpdate.
    void Drain();
}

#endif
