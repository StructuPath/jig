// suggest.go — "did you mean" for an operator's typo: an unknown definition
// field and an unknown CLI command are both named back with the closest
// spelling jig knows, so the fix is in the message.
package protocol

// ClosestMatch returns the candidate nearest to word by edit distance, or ""
// when none is near enough to be the intended spelling. Near enough is at
// most two edits and fewer edits than the word has letters, so a short word
// is not "corrected" into an unrelated one.
func ClosestMatch(word string, candidates []string) string {
	best, bestDistance := "", 3
	for _, candidate := range candidates {
		distance := editDistance(word, candidate)
		if distance < bestDistance && distance < len(word) {
			best, bestDistance = candidate, distance
		}
	}
	return best
}

// editDistance is the Levenshtein distance between a and b, by byte: field
// and command names are ASCII.
func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}
