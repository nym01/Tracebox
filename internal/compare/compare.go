package compare

import "strings"

// Compare returns the test status string for a single output comparison.
func Compare(actual, expected string) string {
	if actual == expected {
		return "accepted"
	}
	if strings.TrimRight(actual, " \t\r\n") == strings.TrimRight(expected, " \t\r\n") {
		return "output_whitespace_mismatch"
	}
	return "wrong_output"
}
