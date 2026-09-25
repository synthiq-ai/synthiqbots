package mcpserver

// GlobMatch is a Go port of ConfigGlobMatch from
// src/mod-ollama-chat_tools.cpp:2642-2669. Supports `*` (zero or more chars)
// and `?` (one char). No character classes; sufficient for config-key names
// like `OllamaChat.Gateway.*` and file globs like `*.conf`.
func GlobMatch(pattern, input string) bool {
	p, s := 0, 0
	starP, starS := -1, 0
	for s < len(input) {
		if p < len(pattern) && (pattern[p] == input[s] || pattern[p] == '?') {
			p++
			s++
		} else if p < len(pattern) && pattern[p] == '*' {
			starP = p
			p++
			starS = s
		} else if starP >= 0 {
			p = starP + 1
			starS++
			s = starS
		} else {
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// MatchesAny returns true if any glob pattern matches the value.
func MatchesAny(value string, patterns []string) bool {
	for _, p := range patterns {
		if GlobMatch(p, value) {
			return true
		}
	}
	return false
}
