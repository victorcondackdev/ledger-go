package main

import (
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	var out strings.Builder
	if err := run(&out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Statement Alice", "R$ 1.425,65", "Card capture Padaria",
		"total  2416.33  2416.33", "balanced", "audit chain verified: 8 transactions",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
