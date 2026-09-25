package config

import "testing"

func TestParseRoots(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]string
	}{
		{"/repo,/configs", map[string]string{"repo": "/repo", "configs": "/configs"}},
		{"r=/x,c=/y", map[string]string{"r": "/x", "c": "/y"}},
		{"  /foo  ,  bar=/baz  ", map[string]string{"foo": "/foo", "bar": "/baz"}},
		{"", map[string]string{}},
	}
	for _, c := range cases {
		got := parseRoots(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("input %q: got %v want %v", c.in, got, c.want)
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Fatalf("input %q: key %q got %q want %q", c.in, k, got[k], v)
			}
		}
	}
}
