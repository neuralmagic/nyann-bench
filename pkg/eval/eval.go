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
