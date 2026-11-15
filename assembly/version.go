package assembly

import "strings"

// NaturalVersionCompare compares two version strings and returns -1, 0 or 1.
//
// Versions frequently look like "2", "10", "1.9.3", "2024-05-01" or
// "v2-beta".  Plain lexicographic ordering puts "10" before "9", so this
// function splits the strings into runs of digits and non-digits and compares
// digit runs numerically.  Leading zeros do not affect the comparison, so
// "01" equals "1".  Non-digit runs are compared case-insensitively first and
// then case-sensitively, so distinct strings never compare equal.  The result
// is a total, deterministic order which is safe for the assembler's version
// checks.
//
// An empty string sorts below any non-empty string.
func NaturalVersionCompare(a, b string) int {
	if a == b {
		return 0
	}
	ai, bi := 0, 0
	for ai < len(a) && bi < len(b) {
		ar, aDigit, an := nextRun(a, ai)
		br, bDigit, bn := nextRun(b, bi)

		var c int
		if aDigit && bDigit {
			c = compareDigitRuns(ar, br)
		} else {
			c = compareTextRuns(ar, br)
		}
		if c != 0 {
			return c
		}
		ai = an
		bi = bn
	}

	switch {
	case ai < len(a):
		return 1
	case bi < len(b):
		return -1
	}
	return 0
}

// nextRun returns the run starting at index i, whether it is a digit run, and
// the index of the first byte after the run.
func nextRun(s string, i int) (run string, digit bool, next int) {
	digit = isASCIIDigit(s[i])
	j := i
	for j < len(s) && isASCIIDigit(s[j]) == digit {
		j++
	}
	return s[i:j], digit, j
}

func isASCIIDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func compareDigitRuns(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return strings.Compare(a, b)
}

func compareTextRuns(a, b string) int {
	al, bl := strings.ToLower(a), strings.ToLower(b)
	if c := strings.Compare(al, bl); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}
