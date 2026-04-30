package eval_test

import (
	"testing"

	"github.com/neuralmagic/nyann-bench/pkg/eval"
)

func TestExtractAnswer(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     string
	}{
		{"hash format", "The answer is #### 42", "42"},
		{"hash with commas", "#### 1,234", "1234"},
		{"boxed", `So the answer is \boxed{18}`, "18"},
		{"last number fallback", "After calculation, the result is 256 apples.", "256"},
		{"negative", "The temperature is #### -5", "-5"},
		{"decimal", "#### 3.14", "3.14"},
		{"hash with trailing text", "#### 42 dollars", "42"},
		{"empty", "", ""},
		{"no numbers", "I don't know the answer.", ""},
		{"deepseek r1 style", " First, find how many clips she sold in May: 48 ÷ 2 = 24.\nThen, add the clips sold in April and May: 48 + 24 = 72.\nSo, Natalia sold 72 clips altogether in April and May.", "72"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eval.ExtractAnswer(tt.response)
			if got != tt.want {
				t.Errorf("ExtractAnswer(%q) = %q, want %q", tt.response, got, tt.want)
			}
		})
	}
}

func TestExtractExpected(t *testing.T) {
	tests := []struct {
		name   string
		answer string
		want   string
	}{
		{"gsm8k format", "Janet sells 9 duck eggs. #### 18", "18"},
		{"number only", "42", "42"},
		{"with commas", "#### 1,000", "1000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eval.ExtractExpected(tt.answer)
			if got != tt.want {
				t.Errorf("ExtractExpected(%q) = %q, want %q", tt.answer, got, tt.want)
			}
		})
	}
}

func TestCheckCorrect(t *testing.T) {
	tests := []struct {
		expected, extracted string
		want                bool
	}{
		{"42", "42", true},
		{"1000", "1,000", true},
		{"42", "43", false},
		{"42", "", false},
		{"", "42", false},
		{"3.0", "3", true},
	}

	for _, tt := range tests {
		t.Run(tt.expected+"_vs_"+tt.extracted, func(t *testing.T) {
			if got := eval.CheckCorrect(tt.expected, tt.extracted); got != tt.want {
				t.Errorf("CheckCorrect(%q, %q) = %v, want %v", tt.expected, tt.extracted, got, tt.want)
			}
		})
	}
}
