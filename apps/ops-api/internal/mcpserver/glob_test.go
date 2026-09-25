package mcpserver

import "testing"

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, input string
		want           bool
	}{
		{"*.conf", "worldserver.conf", true},
		{"*.conf", "worldserver.conf.dist", false},
		{"*.conf*", "worldserver.conf.dist", true},
		{"OllamaChat.Gateway.*", "OllamaChat.Gateway.MaxConcurrent", true},
		{"OllamaChat.Gateway.*", "OllamaChat.Mcp.BearerToken", false},
		{"*Token*", "OllamaChat.Mcp.BearerToken", true},
		{"*Password*", "OllamaChat.DB.Password", true},
		{"?bc", "abc", true},
		{"?bc", "abcd", false},
		{"a*c", "abbbbc", true},
		{"a*c", "ac", true},
		{"", "", true},
		{"*", "anything", true},
		{"exact", "exact", true},
	}
	for _, c := range cases {
		if got := GlobMatch(c.pattern, c.input); got != c.want {
			t.Errorf("GlobMatch(%q,%q) = %v, want %v", c.pattern, c.input, got, c.want)
		}
	}
}

func TestMatchesAny(t *testing.T) {
	denies := []string{"*Token*", "*Password*", "*Secret*"}
	if !MatchesAny("OllamaChat.Mcp.BearerToken", denies) {
		t.Error("expected BearerToken to match deny list")
	}
	if MatchesAny("OllamaChat.Gateway.MaxConcurrent", denies) {
		t.Error("did not expect MaxConcurrent to match deny list")
	}
}
