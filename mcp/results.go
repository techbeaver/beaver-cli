package mcp

// The output shapes tools return.
//
// Every tool has an output schema so a client receives typed structured content
// rather than prose to re-parse. What these types deliberately do NOT do is
// mirror every field of every API response: this package would then be a second
// hand-maintained model of the platform's domain, and two independently
// maintained clients against one API drift. That drift is the failure the
// one-repository decision exists to prevent, so the payload passes through as
// data and only the parts a model must reason about are named.

// ListResult is the shape of every list tool.
type ListResult struct {
	Summary string           `json:"summary" jsonschema:"a one line description of what was found, safe to show the customer"`
	Count   int              `json:"count" jsonschema:"how many items were returned"`
	Items   []map[string]any `json:"items" jsonschema:"the items themselves, exactly as the TechBeaver API returned them"`
}

// ObjectResult is the shape of every tool that returns one thing.
type ObjectResult struct {
	Summary string         `json:"summary" jsonschema:"a one line description of the result, safe to show the customer"`
	Item    map[string]any `json:"item" jsonschema:"the object itself, exactly as the TechBeaver API returned it"`
	// NextStep is set by the few reads whose answer is an instruction rather
	// than a fact. check_repo_access is the case it was added for: "this
	// repository cannot be cloned" is only useful next to "so do not create the
	// app yet, and send the customer this link". Left empty by every read that
	// simply reports something, so an agent never invents a follow-up for a
	// question that was already fully answered.
	NextStep string `json:"nextStep,omitempty" jsonschema:"what has to happen next, if anything; empty when there is nothing to do"`
}

// ActionResult is the shape of every tool that changes something.
type ActionResult struct {
	Summary string         `json:"summary" jsonschema:"what happened, in one line, safe to show the customer"`
	Result  map[string]any `json:"result,omitempty" jsonschema:"whatever the TechBeaver API returned about the change"`
	// NextStep is what the agent or the customer has to do now. Populated
	// whenever finishing the job needs something this tool could not do itself,
	// which for anything involving money is always: a payment link has to be
	// opened by a person.
	NextStep string `json:"nextStep,omitempty" jsonschema:"what has to happen next, if anything, for this to take effect"`
}

// CheckoutResult is what any tool that costs money returns.
//
// It has its own type because the most important thing about it is a negative:
// no tool on this surface can complete a payment. Every checkout path on this
// platform ends at a hosted payment page, so the strongest consent step a
// customer could ask of an AI spending their money is already enforced by the
// architecture rather than by a policy an agent could be talked out of.
type CheckoutResult struct {
	Summary string `json:"summary" jsonschema:"what is being paid for, in one line"`
	// PaymentURL is a hosted payment page. It is the whole point of this type.
	PaymentURL string         `json:"paymentUrl,omitempty" jsonschema:"the payment page the customer must open themselves; you cannot complete this payment for them"`
	Result     map[string]any `json:"result,omitempty" jsonschema:"the invoice or order the TechBeaver API created"`
	NextStep   string         `json:"nextStep" jsonschema:"what to tell the customer to do now"`
}

// DestructiveResult is what a tool that tears something down returns, in both
// of its two states.
//
// The first call returns Status "awaiting_approval" with a preview and a link.
// Only a second call, carrying a confirmation token the account owner approved
// in their browser, returns "done". An agent cannot get from the first to the
// second on its own, which is the guarantee being made to the customer.
type DestructiveResult struct {
	Status  string `json:"status" jsonschema:"awaiting_approval when a person still has to approve this, done when it has happened"`
	Summary string `json:"summary" jsonschema:"what will happen, or what happened, in one line"`

	Preview map[string]any `json:"preview,omitempty" jsonschema:"what would be torn down and what survives, read from the live account"`
	// RecoveryNote states the recovery path and the window in the customer's own
	// terms. Always populated. An agent that deletes something and cannot say
	// how to get it back has turned a recoverable mistake into a panic.
	RecoveryNote string `json:"recoveryNote" jsonschema:"how to recover from this, and by when"`

	ApprovalURL       string `json:"approvalUrl,omitempty" jsonschema:"the link the account owner must open in their browser to approve this"`
	ConfirmationToken string `json:"confirmationToken,omitempty" jsonschema:"pass this back to the same tool once the account owner has approved, to carry the action out"`
	ExpiresAt         string `json:"expiresAt,omitempty" jsonschema:"when the approval link stops working"`

	Result   map[string]any `json:"result,omitempty" jsonschema:"whatever the TechBeaver API returned once the action was carried out"`
	NextStep string         `json:"nextStep" jsonschema:"what to tell the customer to do now"`
}

// LogResult is a bounded tail of a log, never a stream.
//
// Piping a live build log into a model's context is a context-window incident.
// The streaming endpoints stay where they are, for people and for the console.
type LogResult struct {
	Summary   string   `json:"summary" jsonschema:"one line about what was read"`
	Lines     []string `json:"lines" jsonschema:"the last lines of the log, oldest first"`
	Truncated bool     `json:"truncated" jsonschema:"true when older lines were dropped to keep the response small"`
	Source    string   `json:"source" jsonschema:"which log this came from"`
}
