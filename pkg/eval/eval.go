package eval

import (
	"regexp"
	"strings"
)

// numberRe matches integers and decimals, possibly negative, possibly with commas.
var numberRe = regexp.MustCompile(`-?[\d,]+\.?\d*`)

// hashAnswerRe matches "#### <answer>" which is the GSM8K ground truth format.
var hashAnswerRe = regexp.MustCompile(`####\s*(.+)`)

// ExtractAnswer extracts the final numerical answer from a model response.
// It looks for:
//  1. "#### <number>" (GSM8K format)
//  2. "\boxed{<number>}" (LaTeX format)
//  3. The last number in the response (fallback)
func ExtractAnswer(response string) string {
	// Try #### format first — extract the number from what follows ####
	if m := hashAnswerRe.FindStringSubmatch(response); len(m) > 1 {
		after := strings.TrimSpace(m[1])
		if nums := numberRe.FindString(after); nums != "" {
			return normalizeNumber(nums)
		}
		return normalizeNumber(after)
	}

	// Try \boxed{...}
	if idx := strings.LastIndex(response, `\boxed{`); idx >= 0 {
		rest := response[idx+7:]
		if end := strings.Index(rest, "}"); end >= 0 {
			return normalizeNumber(strings.TrimSpace(rest[:end]))
		}
	}

	// Fallback: last number in the response
	matches := numberRe.FindAllString(response, -1)
	if len(matches) > 0 {
		return normalizeNumber(matches[len(matches)-1])
	}

	return ""
}

// ExtractExpected extracts the expected answer from a GSM8K answer field.
// The answer field contains reasoning followed by "#### <number>".
func ExtractExpected(answer string) string {
	if m := hashAnswerRe.FindStringSubmatch(answer); len(m) > 1 {
		after := strings.TrimSpace(m[1])
		if nums := numberRe.FindString(after); nums != "" {
			return normalizeNumber(nums)
		}
		return normalizeNumber(after)
	}
	// If no #### marker, try to get the last number
	matches := numberRe.FindAllString(answer, -1)
	if len(matches) > 0 {
		return normalizeNumber(matches[len(matches)-1])
	}
	return ""
}

// CheckCorrect compares expected and extracted answers.
func CheckCorrect(expected, extracted string) bool {
	if expected == "" || extracted == "" {
		return false
	}
	return normalizeNumber(expected) == normalizeNumber(extracted)
}

// MC answer extraction patterns, ported from OpenAI gpt-oss/evals/abcd_grader.py.
// Priority-ordered: first match by pattern index wins, ties broken by shorter match.
var mcPatterns = []*regexp.Regexp{
	// 0: **Answer:** A  or  *Answers* – B  (markdown-wrapped keyword, bare letter)
	regexp.MustCompile(`(?i)(?:\*{1,2}|_{1,2})Answers?\s*[:\-–]?(?:\*{1,2}|_{1,2})\s*([A-D])\b`),

	// 0.1: Answer: A  at line start with optional markdown
	regexp.MustCompile(`(?im)^\s*(?:\*{1,2}|_{1,2})?Answer:?(?:\*{1,2}|_{1,2})?\s*:?\s*(?:\*{1,2}|_{1,2})?([A-D])(?:\*{1,2}|_{1,2})?\s*`),

	// 1: Answer: (C)  or  Answers: (B)
	regexp.MustCompile(`(?i)\bAnswers?\b\s*[:\-–]?\s*\(\s*([A-D])\s*\)`),

	// 2: Answer: C  or  Answers – D
	regexp.MustCompile(`(?i)\bAnswers?\b\s*[:\-–]?\s*([A-D])\b`),

	// 3: Option B  or  Choice: C
	regexp.MustCompile(`(?i)\b(?:Option|Choice)\b\s*[:\-–]?\s*([A-D])\b`),

	// 7: LaTeX \boxed{...A...}
	regexp.MustCompile(`(?m)\\boxed\{[^}]*?([A-D])[^}]*\}`),

	// 7.5: LaTeX \boxed{\textbf{...C...}}
	regexp.MustCompile(`(?m)\\boxed\{[^}]*?\\textbf\{[^}]*?([A-D])[^}]*\}[^}]*\}`),

	// 7.51: LaTeX \boxed{\text{...C...}}
	regexp.MustCompile(`(?m)\\boxed\{[^}]*?\\text\{[^}]*?([A-D])[^}]*\}[^}]*\}`),

	// 4: bare singletons: (A)  [B]
	regexp.MustCompile(`(?:^|[^A-Za-z0-9])[\(\[]\s*([A-D])\s*[\)\]](?:[^A-Za-z0-9]|$)`),

	// 5: markdown-wrapped: *A*  **B**  _C_  __D__
	regexp.MustCompile(`(?:^|[^A-Za-z0-9])(?:\*{1,2}|_{1,2})([A-D])(?:\*{1,2}|_{1,2})(?:[^A-Za-z0-9]|$)`),

	// 6: LaTeX \textbf{...C...}
	regexp.MustCompile(`\\textbf\{[^}]*?([A-D])[^}]*\}`),

	// 8: **D) description** (markdown-wrapped choice with description)
	regexp.MustCompile(`(?:^|[^A-Za-z0-9])(?:\*{1,2}|_{1,2})\s*([A-D])\)[^*_\n]+?(?:\*{1,2}|_{1,2})(?:[^A-Za-z0-9]|$)`),

	// 9: line that starts with a letter: "A", "B.", "C)", "**D**"
	regexp.MustCompile(`(?m)^\s*(?:\*{1,2}|_{1,2})?([A-D])(?:\*{1,2}|_{1,2})?\s*[.\)\-–:]?`),
}

// ExtractMCAnswer extracts a multiple-choice answer letter from a model response.
//
// For reasoning models (e.g. DeepSeek-R1) that wrap chain-of-thought in
// <think>...</think> tags, extraction runs only on the text after </think>.
//
// Uses the OpenAI gpt-oss/abcd_grader pattern set: priority-ordered regexes
// covering markdown, LaTeX, parenthesized, and bare letter formats.
func ExtractMCAnswer(response string) string {
	// Strip thinking section — only look at the answer part
	text := response
	if idx := strings.LastIndex(response, "</think>"); idx >= 0 {
		text = response[idx+8:]
	}

	type match struct {
		prio     int
		matchLen int
		letter   string
	}

	var matches []match
	for prio, pat := range mcPatterns {
		m := pat.FindStringSubmatch(text)
		if m != nil && len(m) > 1 {
			letter := strings.ToUpper(m[1])
			if letter >= "A" && letter <= "D" {
				matches = append(matches, match{prio, len(m[0]), letter})
			}
		}
	}

	if len(matches) > 0 {
		// Sort by priority (lower first), then by match length (shorter first)
		best := matches[0]
		for _, m := range matches[1:] {
			if m.prio < best.prio || (m.prio == best.prio && m.matchLen < best.matchLen) {
				best = m
			}
		}
		return best.letter
	}

	// Final fallback (matching gpt-oss): first char after stripping **
	stripped := strings.TrimLeft(strings.TrimSpace(text), "*")
	if len(stripped) > 0 {
		ch := strings.ToUpper(string(stripped[0]))
		if ch >= "A" && ch <= "D" {
			return ch
		}
	}

	return ""
}

// IsMCAnswer returns true if the answer looks like a multiple-choice letter: (A)-(D).
var mcCheckRe = regexp.MustCompile(`\(([A-D])\)`)

func IsMCAnswer(answer string) bool {
	return mcCheckRe.MatchString(answer)
}

// CheckMCCorrect compares expected and extracted multiple-choice answers.
func CheckMCCorrect(expected, extracted string) bool {
	if expected == "" || extracted == "" {
		return false
	}
	return normalizeMC(expected) == normalizeMC(extracted)
}

func normalizeMC(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "()")
	return strings.ToUpper(s)
}

// normalizeNumber strips commas and leading zeros from a number string.
func normalizeNumber(s string) string {
	s = strings.ReplaceAll(s, ",", "")
	s = strings.TrimSpace(s)
	// Remove leading zeros but keep "0" and "0.x"
	if len(s) > 1 && s[0] == '0' && s[1] != '.' {
		s = strings.TrimLeft(s, "0")
		if s == "" || s[0] == '.' {
			s = "0" + s
		}
	}
	// Remove trailing .0 or .00 etc
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	return s
}
