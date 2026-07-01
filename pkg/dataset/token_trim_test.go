package dataset

import "testing"

func TestTrimToTokenBudgetKeepsValidUTF8(t *testing.T) {
	text := "alpha café 東京 omega"
	counter := func(s string) (int, error) {
		return len([]rune(s)), nil
	}

	got, trimmed := trimToTokenBudget(text, 11, counter)
	if !trimmed {
		t.Fatal("expected text to be trimmed")
	}
	if got != "alpha café " {
		t.Fatalf("unexpected trim result: %q", got)
	}
}
