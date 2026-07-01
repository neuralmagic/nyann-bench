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

	boundaries := utf8PrefixBoundaries(text)
	lo, hi := 0, len(boundaries)-1
	best := 0
	for lo <= hi {
		mid := (lo + hi) / 2
		prefixLen := boundaries[mid]
		count, err := counter(text[:prefixLen])
		if err != nil {
			break
		}
		if count <= targetTokens {
			best = prefixLen
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

func utf8PrefixBoundaries(text string) []int {
	boundaries := make([]int, 0, len(text)+1)
	boundaries = append(boundaries, 0)
	for i := range text {
		if i > 0 {
			boundaries = append(boundaries, i)
		}
	}
	return append(boundaries, len(text))
}
