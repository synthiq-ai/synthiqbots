// Standalone g++ harness for the leader-playbook relevance filter's pure parts
// (ParsePlaybook / SelectPlaybookRows / RenderPlaybook in mod-ollama-chat_jev_core.h).
// Runs against the REAL prompts/gateway_leader.md so the row count and the
// header/footer split are measured, not assumed. Each -DINJECT_* arm removes
// one guarantee and must FAIL.
//
//   g++ -std=c++17 -I src -I deps tests/jev/harness_playbook.cpp -o /tmp/hp && /tmp/hp prompts/gateway_leader.md
//   -DINJECT_NO_MINROWS   below-threshold rows are not padded up to MinRows  -> FAIL
//   -DINJECT_NO_MAXROWS   MaxRows cap ignored                                 -> FAIL
//   -DINJECT_NO_ORDER     kept rows returned in score order, not file order   -> FAIL

#include "mod-ollama-chat_jev_core.h"

#include <cstdio>
#include <fstream>
#include <sstream>

static int g_failures = 0;
#define CHECK(cond, msg) do { if (!(cond)) { std::printf("FAIL: %s (%s:%d)\n", msg, __FILE__, __LINE__); ++g_failures; } } while (0)

static std::vector<size_t> Select(const std::vector<float>& rel, float minRel, size_t minRows, size_t maxRows)
{
#ifdef INJECT_NO_MINROWS
    minRows = 0;
#endif
#ifdef INJECT_NO_MAXROWS
    maxRows = 0;
#endif
    auto v = Jev::SelectPlaybookRows(rel, minRel, minRows, maxRows);
#ifdef INJECT_NO_ORDER
    std::stable_sort(v.begin(), v.end(), [&](size_t a, size_t b) { return rel[a] > rel[b]; });
#endif
    return v;
}

int main(int argc, char** argv)
{
    // --- synthetic playbook: header, rows, a non-row line in the middle, footer
    {
        std::string text =
            "You are in leader mode.\n\nPlaybook examples:\n"
            "- \"attack\", \"kill it\" -> leader_command_all command=\"attack my target\".\n"
            "- \"follow me\" -> leader_command_all command=\"follow <human name>\".\n"
            "- Role filters can pass through playerbot grammar.\n"
            "- \"hearth\" -> bot_use_hearthstone.\n"
            "Keep replies short.\n";
        Jev::ParsedPlaybook pb = Jev::ParsePlaybook(text);
        CHECK(pb.rows.size() == 3, "three `- \"` rows parsed");
        CHECK(pb.header == "You are in leader mode.\n\nPlaybook examples:\n", "header is everything before the first row");
        CHECK(pb.rows[0].prefix == "\"attack\", \"kill it\"", "prefix is the trigger list before ' -> '");
        CHECK(pb.rows[0].full.rfind("- \"attack\"", 0) == 0, "full row kept verbatim");
        CHECK(pb.footer == "- Role filters can pass through playerbot grammar.\nKeep replies short.\n", "non-row lines after the first row go to the footer, in order");
        std::string out = Jev::RenderPlaybook(pb, {2});
        CHECK(out == pb.header + pb.rows[2].full + "\n" + pb.footer, "render = header + kept rows + footer");
    }

    // --- selection arithmetic
    {
        std::vector<float> rel{0.9f, 0.05f, 0.4f, 0.1f, 0.35f, 0.02f, 0.8f};
        auto k = Select(rel, 0.3f, 2, 10);
        CHECK(k == std::vector<size_t>({0, 2, 4, 6}), "threshold keeps 0.9/0.4/0.35/0.8 in FILE order");   // FAILS under INJECT_NO_ORDER
        auto pad = Select(rel, 0.95f, 3, 10);
        CHECK(pad == std::vector<size_t>({0, 2, 6}), "nothing clears 0.95 -> padded to the top 3 (0.9, 0.8, 0.4)");   // FAILS under INJECT_NO_MINROWS
        auto cap = Select(rel, 0.0f, 1, 2);
        CHECK(cap == std::vector<size_t>({0, 6}), "everything clears 0.0 -> capped to the top 2");                    // FAILS under INJECT_NO_MAXROWS
        auto tiny = Select(std::vector<float>{0.1f}, 0.5f, 4, 12);
        CHECK(tiny == std::vector<size_t>({0}), "fewer rows than MinRows -> returned whole");
        auto none = Select(std::vector<float>{}, 0.5f, 4, 12);
        CHECK(none.empty(), "no rows -> nothing");
    }

    // --- the real playbook, when a path is given
    if (argc > 1)
    {
        std::ifstream f(argv[1]);
        std::stringstream ss; ss << f.rdbuf();
        Jev::ParsedPlaybook pb = Jev::ParsePlaybook(ss.str());
        std::printf("real playbook: %zu rows, header %zu chars, footer %zu chars, total %zu chars\n",
                    pb.rows.size(), pb.header.size(), pb.footer.size(), ss.str().size());
        // Row count is counted independently from the raw text, not a band.
        size_t rawRows = 0; { std::istringstream in(ss.str()); std::string l; while (std::getline(in, l)) if (l.rfind("- \"", 0) == 0) ++rawRows; }
        CHECK(pb.rows.size() == rawRows && rawRows > 0, "every `- \"` line of the file is a parsed row");
        CHECK(pb.header.find("Playbook examples:") != std::string::npos, "header carries the reporting rules and the 'Playbook examples:' line");
        CHECK(pb.footer.find("Keep replies short") != std::string::npos, "footer carries the closing rule");
        size_t maxPrefix = 0, sumPrefix = 0; bool allHaveArrow = true;
        for (const auto& r : pb.rows) { maxPrefix = std::max(maxPrefix, r.prefix.size()); sumPrefix += r.prefix.size(); if (r.full.find(" -> ") == std::string::npos) allHaveArrow = false; }
        CHECK(allHaveArrow, "every row has ' -> '");
        std::printf("prefixes: max %zu chars, total %zu chars (never truncated)\n", maxPrefix, sumPrefix);
        for (const auto& r : pb.rows)
            if (r.full.find(" -> ") != std::string::npos && r.prefix != r.full.substr(2, r.full.find(" -> ") - 2)) { CHECK(false, "prefix is the complete text before ' -> '"); break; }
        // Rendering all rows must reproduce the file EXACTLY, modulo the one
        // documented normalisation: blank lines after the first row are dropped.
        std::string expected; { std::istringstream in(ss.str()); std::string l; bool seen = false;
            while (std::getline(in, l)) { if (!l.empty() && l.back() == '\r') l.pop_back(); if (l.rfind("- \"", 0) == 0) seen = true; if (seen && l.empty()) continue; expected += l + "\n"; } }
        std::vector<size_t> all(pb.rows.size()); for (size_t i = 0; i < all.size(); ++i) all[i] = i;
        std::string full = Jev::RenderPlaybook(pb, all);
        CHECK(full == expected, "render(all) == the file with post-row blank lines dropped (every row and non-row line, in order)");
        // Size of a 12-row render depends on WHICH rows: report the first 12
        // (the shortest, early examples) and the 12 largest, and bound the latter.
        std::vector<size_t> first12 = Jev::SelectPlaybookRows(std::vector<float>(pb.rows.size(), 1.0f), 0.3f, 4, 12);
        std::vector<float> bySize(pb.rows.size()); for (size_t i = 0; i < bySize.size(); ++i) bySize[i] = (float)pb.rows[i].full.size();
        std::vector<size_t> largest12 = Jev::SelectPlaybookRows(bySize, 0.0f, 4, 12);
        size_t szFirst = Jev::RenderPlaybook(pb, first12).size(), szLargest = Jev::RenderPlaybook(pb, largest12).size();
        std::printf("12-row render: first 12 = %zu chars (%.1f%%), largest 12 = %zu chars (%.1f%%), mean row %zu chars\n",
                    szFirst, 100.0 * szFirst / ss.str().size(), szLargest, 100.0 * szLargest / ss.str().size(),
                    pb.rows.empty() ? 0 : (ss.str().size() - pb.header.size() - pb.footer.size()) / pb.rows.size());
        CHECK(first12.size() == 12 && largest12.size() == 12, "both fixtures select exactly 12 rows");
        CHECK(szLargest < ss.str().size() / 2, "even the 12 largest rows render under half the file");
        std::string none = Jev::RenderPlaybook(pb, Jev::SelectPlaybookRows(std::vector<float>(pb.rows.size(), 0.0f), 0.3f, 0, 12));
        CHECK(none == pb.header + pb.footer, "MinRows=0 with nothing relevant -> header + footer only");
    }

    if (g_failures == 0) { std::printf("PASS: harness_playbook\n"); return 0; }
    std::printf("FAIL: harness_playbook (%d)\n", g_failures);
    return 1;
}
