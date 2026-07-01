package dataset

// TokenCounter counts tokens in a string using the target model tokenizer.
type TokenCounter func(string) (int, error)

// trimToTokenBudget returns a prefix of text that is no larger than
// targetTokens according to counter. It prefers the longest prefix known to be
// within budget so callers do not exceed model context limits.
func trimToTokenBudget(text string, targetTokens int, counter TokenCounter) (string, bool) {
	if counter == nil || targetTokens <= 0 || len(text) == 0 {
		return text, false
	}

	count, err := counter(text)
	if err != nil || count <= targetTokens {
		return text, false
	}

	lo, hi := 0, len(text)
	best := 0
	for lo <= hi {
		mid := (lo + hi) / 2
		count, err := counter(text[:mid])
		if err != nil {
			break
		}
		if count <= targetTokens {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}

	if best == 0 {
		return "", true
	}
	return text[:best], true
}
