package dataset

import (
	"testing"
	"unicode/utf8"
)

func TestTrimToTokenBudgetKeepsValidUTF8(t *testing.T) {
	text := "alpha café 東京 omega"
	calls := 0
	counter := func(s string) (int, error) {
		calls++
		return len([]rune(s)), nil
	}

	got, trimmed := trimToTokenBudget(text, 11, counter)
	if !trimmed {
		t.Fatal("expected text to be trimmed")
	}
	if !utf8.ValidString(got) {
		t.Fatalf("trimmed text is not valid UTF-8: %q", got)
	}
	if len([]rune(got)) > 11 {
		t.Fatalf("trimmed text has %d tokens, want <= 11", len([]rune(got)))
	}
	if calls > 7 {
		t.Fatalf("token counter called %d times, want <= 7", calls)
	}
}
