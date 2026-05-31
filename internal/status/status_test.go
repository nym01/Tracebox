package status

import "testing"

func TestTopLevel(t *testing.T) {
	cases := []struct {
		name         string
		buildStatus  string
		testStatuses []string
		want         string
	}{
		{
			name:        "build failed, no tests",
			buildStatus: BuildFailed,
			want:        "build_failed",
		},
		{
			name:         "build failed, tests present",
			buildStatus:  BuildFailed,
			testStatuses: []string{Accepted, WrongOutput},
			want:         "build_failed",
		},
		{
			name:        "build internal_error",
			buildStatus: BuildInternalError,
			want:        InternalError,
		},
		{
			name:         "build ok, all accepted",
			buildStatus:  BuildOK,
			testStatuses: []string{Accepted, Accepted, Accepted},
			want:         Accepted,
		},
		{
			name:         "build ok, empty test list",
			buildStatus:  BuildOK,
			testStatuses: []string{},
			want:         Accepted,
		},
		{
			name:         "build ok, first test wrong_output",
			buildStatus:  BuildOK,
			testStatuses: []string{WrongOutput, Accepted},
			want:         WrongOutput,
		},
		{
			name:         "build ok, first accepted second time_exceeded",
			buildStatus:  BuildOK,
			testStatuses: []string{Accepted, TimeExceeded},
			want:         TimeExceeded,
		},
		{
			name:         "build ok, accepted then runtime_error then wrong_output",
			buildStatus:  BuildOK,
			testStatuses: []string{Accepted, RuntimeError, WrongOutput},
			want:         RuntimeError,
		},
		{
			name:         "build ok, internal_error test",
			buildStatus:  BuildOK,
			testStatuses: []string{InternalError},
			want:         InternalError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TopLevel(tc.buildStatus, tc.testStatuses)
			if got != tc.want {
				t.Errorf("TopLevel(%q, %v) = %q, want %q", tc.buildStatus, tc.testStatuses, got, tc.want)
			}
		})
	}
}
