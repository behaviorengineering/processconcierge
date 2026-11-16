package main

import "testing"

func TestResolveVersion(t *testing.T) {
	cases := []struct{ ldflag, module, want string }{
		{"v1.2.3", "v1.2.0", "v1.2.3"},
		{"dev", "v0.1.0", "v0.1.0"},
		{"dev", "(devel)", "dev"},
	}
	for _, c := range cases {
		if got := resolveVersion(c.ldflag, c.module); got != c.want {
			t.Errorf("resolveVersion(%q, %q) = %q, want %q", c.ldflag, c.module, got, c.want)
		}
	}
}
