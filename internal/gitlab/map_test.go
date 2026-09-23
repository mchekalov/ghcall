package gitlab

import "testing"

func TestMRState(t *testing.T) {
	cases := map[string]string{
		"opened": "OPEN",
		"merged": "MERGED",
		"closed": "CLOSED",
		"locked": "CLOSED",
		// GitLab's GraphQL emits lowercase, but be indifferent to case.
		"OPENED": "OPEN",
		// Unknown states pass through rather than being coerced into one
		// ghcall knows: an unmatched state is visible, a wrong one is not.
		"something_new": "something_new",
		"":              "",
	}
	for in, want := range cases {
		if got := MRState(in); got != want {
			t.Errorf("MRState(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPipelineState(t *testing.T) {
	cases := map[string]string{
		"SUCCESS": "SUCCESS",
		"FAILED":  "FAILURE",

		"CREATED":              "PENDING",
		"WAITING_FOR_RESOURCE": "PENDING",
		"PREPARING":            "PENDING",
		"PENDING":              "PENDING",
		"RUNNING":              "PENDING",
		"SCHEDULED":            "PENDING",
		"MANUAL":               "PENDING",

		// Deliberately not ERROR: a cancelled or skipped pipeline is not a
		// broken build, and must never trip the autofix agent.
		"CANCELED":  "CANCELED",
		"SKIPPED":   "SKIPPED",
		"CANCELING": "CANCELING",

		"": "",
	}
	for in, want := range cases {
		if got := PipelineState(in); got != want {
			t.Errorf("PipelineState(%q) = %q, want %q", in, got, want)
		}
	}

	for _, s := range []string{"CANCELED", "SKIPPED", "CANCELING"} {
		if got := PipelineState(s); got == "FAILURE" || got == "ERROR" {
			t.Errorf("PipelineState(%q) = %q, which the agent would treat as a failing build", s, got)
		}
	}
}

func TestMRStateArg(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"OPEN"}, "opened"},
		{[]string{"CLOSED", "MERGED"}, "all"},
		{[]string{"OPEN", "CLOSED", "MERGED"}, "all"},
		{nil, "all"},
	}
	for _, tc := range cases {
		if got := mrStateArg(tc.in); got != tc.want {
			t.Errorf("mrStateArg(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
