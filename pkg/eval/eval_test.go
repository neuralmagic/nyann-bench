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

func TestExtractMCAnswer(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     string
	}{
		{"strict", "After thinking step by step, The answer is (B).", "B"},
		{"strict no parens", "Answer: C", "C"},
		{"the answer is parens", "The answer is (C).", "C"},
		{"flexible", "I think (A) is correct because...", "A"},
		{"flexible later", "Let me consider all options. Looking at (D), it seems right.", "D"},
		{"cot with answer", "Step 1: ... Step 2: ... Therefore, the Answer: (C).", "C"},
		{"r1 think tags", "<think>\nLet me analyze each option.\n(A) seems plausible\n(B) is wrong\n</think>\n\n**Final Answer: A**", "A"},
		{"r1 final answer bold", "Some reasoning here.\n\nAnswer: C", "C"},
		{"r1 with explanation after think", "<think>long reasoning (A) mentioned (B) also (C) discussed</think>\n\nHemoglobin transports oxygen.\n- Option (A) is correct\n- Option (B) is wrong\n\n**Answer:** A", "A"},
		{"r1 answer is format", "<think>thinking about (B) and (C)</think>\n\nAnswer: (D)", "D"},
		{"r1 bold answer", "<think>reasoning</think>\n\n**Answer: B**", "B"},
		{"r1 bare letter", "<think>reasoning</think>\n\nA", "A"},
		{"boxed letter", `The answer is \boxed{B}`, "B"},
		{"boxed textbf", `\boxed{\textbf{C}}`, "C"},
		{"boxed text", `\boxed{\text{A}}`, "A"},
		{"textbf", `\textbf{D}`, "D"},
		{"markdown bold letter", "**B**", "B"},
		{"markdown italic letter", "*C*", "C"},
		{"option keyword", "Option B is correct", "B"},
		{"choice keyword", "Choice: D", "D"},
		{"bracket notation", "[A]", "A"},
		{"answer with dash", "Answer – B", "B"},
		{"bold answer keyword", "**Answer:** C", "C"},
		{"bold answer with bold letter", "**Answer: D**", "D"},
		{"markdown d paren desc", "**D) Hemoglobin filters waste**", "D"},
		{"first match wins", "Looking at (A) and (B), considering (C), I believe (D) is correct.", "A"},
		{"no answer", "I'm not sure about any of these.", ""},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eval.ExtractMCAnswer(tt.response)
			if got != tt.want {
				t.Errorf("ExtractMCAnswer(%q) = %q, want %q", tt.response, got, tt.want)
			}
		})
	}
}

func TestIsMCAnswer(t *testing.T) {
	tests := []struct {
		answer string
		want   bool
	}{
		{"(A)", true},
		{"(D)", true},
		{"42", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.answer, func(t *testing.T) {
			if got := eval.IsMCAnswer(tt.answer); got != tt.want {
				t.Errorf("IsMCAnswer(%q) = %v, want %v", tt.answer, got, tt.want)
			}
		})
	}
}
