#include "mod-ollama-chat_worldtask.h"

#include <chrono>
#include <condition_variable>
#include <deque>
#include <memory>
#include <mutex>
#include <thread>

namespace OllamaChat::WorldTask
{
    namespace
    {
        struct Task
        {
            std::function<nlohmann::json()> fn;
            std::mutex m;
            std::condition_variable cv;
            nlohmann::json result;
            bool done = false;
        };

        std::mutex s_queueMutex;
        std::deque<std::shared_ptr<Task>> s_queue;

        // The thread that drains the queue IS the world thread. A Run() issued
        // from it must execute inline: waiting would block the very update that
        // drains the queue (a 500 ms stall per call, then a timeout).
        std::mutex s_worldIdMutex;
        std::thread::id s_worldThreadId;
        bool s_haveWorldThreadId = false;

        bool OnWorldThread()
        {
            std::lock_guard<std::mutex> lk(s_worldIdMutex);
            return s_haveWorldThreadId && std::this_thread::get_id() == s_worldThreadId;
        }
    }

    nlohmann::json Run(std::function<nlohmann::json()> fn, uint32_t timeoutMs)
    {
        if (OnWorldThread())
        {
            try { return fn(); }
            catch (const std::exception& e) { return nlohmann::json{{"error", std::string("world task threw: ") + e.what()}}; }
            catch (...) { return nlohmann::json{{"error", "world task threw"}}; }
        }
        auto task = std::make_shared<Task>();
        task->fn = std::move(fn);
        {
            std::lock_guard<std::mutex> lk(s_queueMutex);
            s_queue.push_back(task);
        }
        std::unique_lock<std::mutex> lk(task->m);
        if (!task->cv.wait_for(lk, std::chrono::milliseconds(timeoutMs), [&] { return task->done; }))
            return nlohmann::json{{"error", "timed out waiting for the world thread (" + std::to_string(timeoutMs) + " ms)"}};
        return task->result;
    }

    void Drain()
    {
        {
            std::lock_guard<std::mutex> lk(s_worldIdMutex);
            if (!s_haveWorldThreadId)
            {
                s_worldThreadId = std::this_thread::get_id();
                s_haveWorldThreadId = true;
            }
        }
        std::deque<std::shared_ptr<Task>> batch;
        {
            std::lock_guard<std::mutex> lk(s_queueMutex);
            batch.swap(s_queue);
        }
        for (auto& task : batch)
        {
            nlohmann::json r;
            try { r = task->fn(); }
            catch (const std::exception& e) { r = nlohmann::json{{"error", std::string("world task threw: ") + e.what()}}; }
            catch (...) { r = nlohmann::json{{"error", "world task threw"}}; }
            {
                std::lock_guard<std::mutex> lk(task->m);
                task->result = std::move(r);
                task->done = true;
            }
            task->cv.notify_all();
        }
    }
}
