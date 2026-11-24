package maintenance

import "testing"

func TestShortDate(t *testing.T) {
	cases := map[string]string{
		"2026-11-16": "16 nov",
		"2026-01-01": "1 ene",
		"2026-12-31": "31 dic",
		"no-fecha":   "no-fecha",
	}
	for in, want := range cases {
		if got := shortDate(in); got != want {
			t.Errorf("shortDate(%q) = %q, quería %q", in, got, want)
		}
	}
}
