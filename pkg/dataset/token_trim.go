package dataset

import "unicode/utf8"

// TokenCounter counts tokens in a string using the target model tokenizer.
type TokenCounter func(string) (int, error)

// trimToTokenBudget uses a bounded number of remote token counts to trim text
// toward targetTokens. The bounded fallback is approximate.
func trimToTokenBudget(text string, targetTokens int, counter TokenCounter) (string, bool) {
	if counter == nil || targetTokens <= 0 || len(text) == 0 {
		return text, false
	}

	count, err := counter(text)
	if err != nil || count <= targetTokens {
		return text, false
	}
	return trimToTokenBudgetWithCount(text, targetTokens, count, counter), true
}

// trimToTokenBudgetWithCount proportionally converges on targetTokens using a
// bounded number of calls to the remote token counter. count must be the token
// count for text and greater than targetTokens.
func trimToTokenBudgetWithCount(text string, targetTokens, count int, counter TokenCounter) string {
	const maxAttempts = 6

	source := text
	highBytes, highCount := len(source), count
	lowBytes, lowCount := 0, 0
	var best string

	for range maxAttempts {
		denominator := highCount - lowCount
		if denominator <= 0 {
			break
		}
		keep := lowBytes + (targetTokens-lowCount)*(highBytes-lowBytes)/denominator
		candidate := utf8SafePrefix(source, keep)
		if candidate == "" {
			return candidate
		}

		candidateCount, err := counter(candidate)
		if err != nil {
			if best != "" {
				return best
			}
			return candidate
		}
		if candidateCount == targetTokens {
			return candidate
		}
		if candidateCount < targetTokens {
			best = candidate
			lowBytes, lowCount = len(candidate), candidateCount
		} else {
			highBytes, highCount = len(candidate), candidateCount
		}
	}

	if best != "" {
		return best
	}
	// No candidate was known to fit. Make one final conservative adjustment
	// from the smallest measured over-budget prefix without another round trip.
	return utf8SafePrefix(source, highBytes*targetTokens/highCount)
}

func utf8SafePrefix(text string, keep int) string {
	if keep >= len(text) {
		return text
	}
	for keep > 0 && !utf8.RuneStart(text[keep]) {
		keep--
	}
	return text[:keep]
}
