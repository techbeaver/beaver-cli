// Package approval interprets the status of a human-approval request, so the
// CLI and the MCP server can never disagree about what a status means.
package approval

// Outcome is what a confirmation status means for a caller that is waiting.
type Outcome int

const (
	// Unknown is the zero value: a status this version does not recognise. It is never approval.
	Unknown Outcome = iota
	// Pending means nobody has decided yet, so keep waiting.
	Pending
	// Approved means a person said yes.
	Approved
	// Rejected means a person said no.
	Rejected
	// Consumed means the approval was already spent on an earlier call.
	Consumed
	// Expired means the request timed out before anyone decided.
	Expired
)

// Classify maps a status string from the API to an Outcome. Matching is exact:
// a status that differs in case or spelling is Unknown, never Approved.
func Classify(status string) Outcome {
	switch status {
	case "approved":
		return Approved
	case "pending":
		return Pending
	case "rejected", "declined":
		return Rejected
	case "consumed":
		return Consumed
	case "expired":
		return Expired
	default:
		return Unknown
	}
}

// Settled reports whether waiting longer can change the answer.
func (o Outcome) Settled() bool {
	return o != Pending
}
