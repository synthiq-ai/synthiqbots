#!/usr/bin/env python3
"""
Extract SQL query strings from mod-ollama-chat C++ sources for live-schema validation.

The module's ad-hoc database queries land in calls like

    WorldDatabase.Query(sql)
    CharacterDatabase.Query("SELECT ...")
    LoginDatabase.Query(fmt::format("SELECT ...", ...))

The query argument is one of:
  1. A bare string literal (possibly multi-line via C++ adjacent-string concat).
  2. A `fmt::format(...)` call where the first arg is the literal SQL template
     and the remaining args fill `{}` placeholders.
  3. A local `std::string sql = ...` built from one of the above, then passed by name.

For each query found, we emit one line of SQL to stdout, with `{}` placeholders
replaced by `0` (safe for EXPLAIN against any int column). Backticks and other
identifiers are preserved.

Output is consumed by `smoke-sql-against-live-db.sh` which runs each as
`EXPLAIN <sql>` against the live AC schema. EXPLAIN is read-only and parses
the query the same way SELECT does, so column-name typos surface as
`[1054] Unknown column`.

LIMITATIONS — intentionally underapproximating to avoid false positives:
  - Only catches inlined string-literal SQL or one-step `std::string sql =`
    + `XxxDatabase.Query(sql)` patterns.
  - Skips queries built from variables (where the SQL itself is dynamic).
  - Skips queries with .Query() calls split across many lines/heredoc — those
    are visually inspectable anyway. Coverage > precision; this is a smoke
    test, not a static analyzer.
"""
import os
import re
import sys

REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
SRC_DIR = os.path.join(REPO_ROOT, "src")

# Regex to find Query call sites. We capture the whole .Query( ... ) line span.
# Permissive: any of WorldDatabase / CharacterDatabase / LoginDatabase.
QUERY_CALL_RE = re.compile(
    r"(?:World|Character|Login)Database\.Query\s*\(\s*(.*?)\s*\)\s*;",
    re.DOTALL,
)

# Match a sequence of adjacent C++ string literals (the typical SQL builder),
# allowing optional whitespace/newlines between them. Each literal is a "..."
# possibly preceded by R"(...)" raw form.
ADJACENT_STRINGS_RE = re.compile(
    r"""
    \s*
    (?:
        R"\([^)]*\)"          # raw string literal R"(...)"
      | "(?:[^"\\]|\\.)*"     # normal string literal "..."
    )
    (?:\s*
        (?:
            R"\([^)]*\)"
          | "(?:[^"\\]|\\.)*"
        )
    )*
    \s*
    """,
    re.VERBOSE,
)

# Inside a captured string-literal chain, pull out the individual literal
# contents and concatenate them.
LITERAL_RE = re.compile(
    r"""
    R"\(([^)]*)\)"        # raw form, capture inside
    | "((?:[^"\\]|\\.)*)" # normal form, capture inside
    """,
    re.VERBOSE,
)

# Find `std::string NAME = ... ;` or `std::string NAME = fmt::format( "..."
# (multi-line). We only handle the simple "= followed by a string-literal-chain"
# case AND the "= fmt::format( first-arg )" case.
LOCAL_SQL_DECL_RE = re.compile(
    r"std::string\s+(\w+)\s*=\s*(.*?);\s*\n",
    re.DOTALL,
)


def _join_literals(text: str) -> str | None:
    """Given a snippet that *might* be a chain of C++ string literals, return
    the concatenated contents. Returns None if no literal found.

    Only literals that are GENUINELY adjacent — separated by nothing but
    whitespace/newlines, which is the sole way C++ implicit string
    concatenation works — are joined. We stop at the first literal preceded by
    intervening code, so two independent queries are never fused into one
    syntactically broken string. This matters when the greedy `.Query(...)`
    call regex spans a ternary like

        QueryResult r = cond ? CharacterDatabase.Query("SELECT a ...")
                             : CharacterDatabase.Query("SELECT b ...");

    where the `") : CharacterDatabase.Query("` between the two arms' literals
    is real code, not whitespace — joining them produced garbage SQL
    (`...SELECT a ...SELECT b ...`) and a false EXPLAIN syntax error."""
    matches = list(LITERAL_RE.finditer(text))
    if not matches:
        return None
    parts = [matches[0].group(1) if matches[0].group(1) is not None else matches[0].group(2)]
    prev_end = matches[0].end()
    for m in matches[1:]:
        if text[prev_end:m.start()].strip() != "":
            break  # intervening non-string code → not an adjacent-literal concat
        parts.append(m.group(1) if m.group(1) is not None else m.group(2))
        prev_end = m.end()
    return "".join(parts)


def _extract_first_format_arg(call_body: str) -> str | None:
    """Given the contents of `fmt::format( ... )`, return the joined first
    arg if it's a string-literal chain. We crudely scan up to the first
    top-level comma."""
    depth = 0
    first_arg = []
    for ch in call_body:
        if ch == "," and depth == 0:
            break
        first_arg.append(ch)
        if ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
    return _join_literals("".join(first_arg))


def _scan_file(path: str):
    """Yield (line_no, sql_text) tuples for each Query() call in the file."""
    with open(path, "r", encoding="utf-8", errors="replace") as f:
        src = f.read()

    # Pre-index `std::string sql = ...` declarations so we can resolve
    # `Database.Query(sql)` when the literal is built one statement earlier.
    locals_map: dict[str, str] = {}
    for m in LOCAL_SQL_DECL_RE.finditer(src):
        name, rhs = m.group(1), m.group(2)
        # Case A: rhs is an adjacent string-literal chain.
        joined = _join_literals(rhs)
        if joined is None:
            # Case B: rhs starts with fmt::format( first-arg, ... ).
            fmt_match = re.match(r"\s*fmt::format\s*\(\s*(.*)$", rhs, re.DOTALL)
            if fmt_match:
                joined = _extract_first_format_arg(fmt_match.group(1))
        if joined is not None:
            locals_map[name] = joined

    for m in QUERY_CALL_RE.finditer(src):
        body = m.group(1).strip()
        line_no = src.count("\n", 0, m.start()) + 1

        # Case 1: arg is a single identifier we resolved earlier.
        ident_match = re.fullmatch(r"\w+", body)
        if ident_match and body in locals_map:
            sql = locals_map[body]
        # Case 2: arg is `fmt::format( "...", ... )` directly.
        elif body.startswith("fmt::format"):
            inner = re.match(r"fmt::format\s*\(\s*(.*)$", body, re.DOTALL)
            sql = _extract_first_format_arg(inner.group(1)) if inner else None
        # Case 3: arg is an inline string-literal chain.
        else:
            sql = _join_literals(body)

        if sql is None:
            continue
        # Replace fmt placeholders with `0` — safe for EXPLAIN of any int col,
        # and harmless for string contexts (EXPLAIN doesn't care about value).
        sql = re.sub(r"\{[^{}]*\}", "0", sql)
        # Collapse internal whitespace; EXPLAIN doesn't care.
        sql = " ".join(sql.split())
        yield (line_no, sql)


def main():
    for dirpath, dirnames, filenames in os.walk(SRC_DIR):
        dirnames[:] = [d for d in dirnames if not d.startswith(".") and d != "build"]
        for name in sorted(filenames):
            if not name.endswith((".cpp", ".h", ".hpp")):
                continue
            path = os.path.join(dirpath, name)
            rel = os.path.relpath(path, REPO_ROOT)
            for line_no, sql in _scan_file(path):
                # Output line format: "<rel-path>:<line>\t<sql>"
                # Tab separator chosen because SQL never contains literal tabs.
                print(f"{rel}:{line_no}\t{sql}")


if __name__ == "__main__":
    main()
