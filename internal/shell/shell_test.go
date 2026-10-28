package shell

import (
	"testing"
)

func TestShellSplit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`a b  c`, []string{"a", "b", "c"}},
		{`'single quoted' "double \"quoted\"" back\ slash`, []string{"single quoted", `double "quoted"`, "back slash"}},
		{`sh -c 'x '\''y'\''' sh 'line1` + "\n" + `line2'`, []string{"sh", "-c", "x 'y'", "sh", "line1\nline2"}},
		{`"a\$b\q"`, []string{`a$b\q`}},
		{``, nil},
	}
	for _, tc := range cases {
		got := Split(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("Split(%q) = %q, want %q", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("Split(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestShellQuoteRoundTrip(t *testing.T) {
	for _, s := range []string{"plain", "/usr/local/bin/comlink", "has space", "it's", "", "a\nb", `back\slash`, "$HOME"} {
		got := Split(Quote(s))
		if s == "" {
			if len(got) != 1 || got[0] != "" {
				t.Errorf("Quote(%q) round-trips to %q", s, got)
			}
			continue
		}
		if len(got) != 1 || got[0] != s {
			t.Errorf("Quote(%q) = %q round-trips to %q", s, Quote(s), got)
		}
	}
}
