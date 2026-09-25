#ifndef MOD_OLLAMA_CHAT_COMMAND_H
#define MOD_OLLAMA_CHAT_COMMAND_H

#include "ScriptMgr.h"
#include "Chat.h"

class OllamaChatConfigCommand : public CommandScript
{
public:
    OllamaChatConfigCommand();
    Acore::ChatCommands::ChatCommandTable GetCommands() const override;

    static bool HandleOllamaReloadCommand(ChatHandler* handler);
    static bool HandleOllamaSentimentViewCommand(ChatHandler* handler, Optional<std::string> botName, Optional<std::string> playerName);
    static bool HandleOllamaSentimentSetCommand(ChatHandler* handler, std::string botName, std::string playerName, float sentimentValue);
    static bool HandleOllamaSentimentResetCommand(ChatHandler* handler, Optional<std::string> botName, Optional<std::string> playerName);
    static bool HandleOllamaPersonalityGetCommand(ChatHandler* handler, std::string botName);
    static bool HandleOllamaPersonalitySetCommand(ChatHandler* handler, std::string botName, std::string personality);
    static bool HandleOllamaPersonalityListCommand(ChatHandler* handler);
    static bool HandleOllamaGatewayStatusCommand(ChatHandler* handler);
    static bool HandleOllamaGatewayTestCommand(ChatHandler* handler, std::string botName, Acore::ChatCommands::Tail prompt);
    static bool HandleOllamaGatewayCostsCommand(ChatHandler* handler, Optional<uint32> hours);
    static bool HandleOllamaGatewayPruneCommand(ChatHandler* handler);
    static bool HandleOllamaTacticalStatusCommand(ChatHandler* handler, Optional<uint64> botGuid);

    // Player-facing controls (SEC_PLAYER)
    static bool HandleOllamaOptOutCommand(ChatHandler* handler);
    static bool HandleOllamaOptInCommand(ChatHandler* handler);
    static bool HandleOllamaMuteCommand(ChatHandler* handler, std::string botName);
    static bool HandleOllamaUnmuteCommand(ChatHandler* handler, std::string botName);
};

#endif // MOD_OLLAMA_CHAT_COMMAND_H
