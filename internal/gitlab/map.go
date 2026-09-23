package gitlab

import "strings"

// MRState maps a GitLab merge request state onto ghcall's canonical PR
// vocabulary. `locked` is a merge in progress, which for reporting purposes
// is no longer open, so it lands on CLOSED alongside `closed`.
//
// Anything unrecognised is passed through unchanged rather than coerced:
// a state ghcall does not know about simply fails to match any filter,
// which is visible, instead of silently masquerading as one it does know.
func MRState(s string) string {
	switch strings.ToLower(s) {
	case "opened":
		return "OPEN"
	case "merged":
		return "MERGED"
	case "closed", "locked":
		return "CLOSED"
	default:
		return s
	}
}

// PipelineState maps a GitLab pipeline status onto ghcall's canonical CI
// vocabulary (the statusCheckRollup values GitHub reports).
//
// CANCELED and SKIPPED are deliberately *not* mapped to ERROR: the agent
// trigger treats FAILURE and ERROR as "this PR needs fixing", and a
// cancelled or skipped pipeline is not a broken build. They pass through
// as-is, matching no failing state, as does any status a newer GitLab
// introduces.
func PipelineState(s string) string {
	switch strings.ToUpper(s) {
	case "":
		return ""
	case "SUCCESS":
		return "SUCCESS"
	case "FAILED":
		return "FAILURE"
	case "CREATED", "WAITING_FOR_RESOURCE", "PREPARING", "PENDING",
		"RUNNING", "SCHEDULED", "MANUAL":
		return "PENDING"
	default:
		return s
	}
}

// mrStateArg picks the single `state:` argument for a mergeRequests query
// from the canonical states a filter asked for. GitLab's GraphQL takes one
// state, not a list, so anything other than "open alone" asks for `all` and
// lets the pipeline's own state matching narrow it down.
func mrStateArg(states []string) string {
	if len(states) == 1 && strings.ToUpper(states[0]) == "OPEN" {
		return "opened"
	}
	return "all"
}
