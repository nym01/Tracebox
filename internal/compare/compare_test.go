package compare

import "testing"

func TestCompare(t *testing.T) {
	tests := []struct {
		name     string
		actual   string
		expected string
		want     string
	}{
		{
			name:     "exact match",
			actual:   "hello\n",
			expected: "hello\n",
			want:     "accepted",
		},
		{
			name:     "exact match empty",
			actual:   "",
			expected: "",
			want:     "accepted",
		},
		{
			name:     "trailing newline differs",
			actual:   "hello\n",
			expected: "hello",
			want:     "output_whitespace_mismatch",
		},
		{
			name:     "trailing spaces differ",
			actual:   "hello   ",
			expected: "hello",
			want:     "output_whitespace_mismatch",
		},
		{
			name:     "trailing whitespace on both sides",
			actual:   "hello \n",
			expected: "hello\n\n",
			want:     "output_whitespace_mismatch",
		},
		{
			name:     "different content",
			actual:   "hello\n",
			expected: "world\n",
			want:     "wrong_output",
		},
		{
			name:     "different content no trailing whitespace",
			actual:   "42",
			expected: "43",
			want:     "wrong_output",
		},
		{
			name:     "whitespace in middle differs",
			actual:   "a b\n",
			expected: "a  b\n",
			want:     "wrong_output",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Compare(tc.actual, tc.expected)
			if got != tc.want {
				t.Errorf("Compare(%q, %q) = %q, want %q", tc.actual, tc.expected, got, tc.want)
			}
		})
	}
}
