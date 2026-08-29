// Package scopes is the vocabulary a machine credential is granted in.
//
// Two invariants hold and are enforced by tests. There are no admin scopes, so
// no combination of grants reaches an administrative route. A scope that is
// defined is not necessarily issuable: see [Issuable].
//
// Rationale is in ADR 0013.
package scopes

import (
	"sort"
	"strings"
)

// The scope vocabulary.
const (
	ProjectsRead  = "projects:read"
	ProjectsWrite = "projects:write"

	PaaSRead    = "paas:read"
	PaaSWrite   = "paas:write"
	PaaSDeploy  = "paas:deploy"
	PaaSDestroy = "paas:destroy"

	DBaaSRead    = "dbaas:read"
	DBaaSWrite   = "dbaas:write"
	DBaaSDestroy = "dbaas:destroy"

	LogsRead = "logs:read"

	BillingRead     = "billing:read"
	BillingCheckout = "billing:checkout"

	// BillingSpend is defined and enforced but not issuable. See ADR 0013.
	BillingSpend = "billing:spend"
)

// All is every scope this system understands, in the order a consent screen
// should list them.
var All = []string{
	ProjectsRead, ProjectsWrite,
	PaaSRead, PaaSWrite, PaaSDeploy, PaaSDestroy,
	DBaaSRead, DBaaSWrite, DBaaSDestroy,
	LogsRead,
	BillingRead, BillingCheckout, BillingSpend,
}

// Default is what a client gets when it asks for nothing in particular. It
// excludes both destroy scopes and BillingSpend.
var Default = []string{
	ProjectsRead, ProjectsWrite,
	PaaSRead, PaaSWrite, PaaSDeploy,
	DBaaSRead, DBaaSWrite,
	LogsRead,
	BillingRead, BillingCheckout,
}

var destructive = map[string]bool{
	PaaSDestroy:  true,
	DBaaSDestroy: true,
}

var notIssuable = map[string]bool{
	BillingSpend: true,
}

var descriptions = map[string]string{
	ProjectsRead:    "See your projects and the regions available to you",
	ProjectsWrite:   "Create projects",
	PaaSRead:        "See your apps, their settings and their environment variables",
	PaaSWrite:       "Create apps, change their settings, and start or stop them",
	PaaSDeploy:      "Deploy your apps and roll a deployment back",
	PaaSDestroy:     "Delete apps and custom domains. Every deletion still needs you to approve it in your browser",
	DBaaSRead:       "See your managed databases, their settings and their usage",
	DBaaSWrite:      "Create managed databases, databases inside them, roles and backups",
	DBaaSDestroy:    "Delete managed databases and restore over their data. Every deletion still needs you to approve it in your browser",
	LogsRead:        "Read your build and runtime logs",
	BillingRead:     "See your invoices, credit balance and what a purchase would cost",
	BillingCheckout: "Prepare a payment. It cannot complete one: only you can, on the payment page",
	BillingSpend:    "Spend your account credit against an invoice without a payment page",
}

// Describe returns the consent-screen wording for a scope, or the scope itself
// if it has none.
func Describe(scope string) string {
	if d, ok := descriptions[scope]; ok {
		return d
	}
	return scope
}

// IsKnown reports whether the scope is in the vocabulary.
func IsKnown(scope string) bool {
	_, ok := descriptions[scope]
	return ok
}

// IsDestructive reports whether granting this scope permits destroying
// something. Consent screens must present these separately.
func IsDestructive(scope string) bool { return destructive[scope] }

// Issuable reports whether a scope may currently be granted to a token. A known
// scope that is not issuable is a deliberate state.
func Issuable(scope string) bool { return IsKnown(scope) && !notIssuable[scope] }

// Issuables is All minus the scopes nothing may grant.
func Issuables() []string {
	out := make([]string, 0, len(All))
	for _, s := range All {
		if Issuable(s) {
			out = append(out, s)
		}
	}
	return out
}

// Parse turns a space- or comma-separated scope string into a normalised,
// de-duplicated, sorted list, and reports any entry it did not recognise.
// Unknown entries are returned rather than dropped.
func Parse(raw string) (known []string, unknown []string) {
	seen := map[string]bool{}
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ' ' || r == ',' || r == '\t' || r == '\n'
	}) {
		s := strings.TrimSpace(field)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		if IsKnown(s) {
			known = append(known, s)
		} else {
			unknown = append(unknown, s)
		}
	}
	sort.Strings(known)
	sort.Strings(unknown)
	return known, unknown
}

// Normalise de-duplicates and sorts a scope list, dropping anything unknown.
// Use it on stored data; use [Parse] on anything a client sent.
func Normalise(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] || !IsKnown(s) {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Join renders a scope list the way OAuth carries it: space separated.
func Join(in []string) string { return strings.Join(in, " ") }

// Has reports whether granted contains want.
func Has(granted []string, want string) bool {
	for _, g := range granted {
		if g == want {
			return true
		}
	}
	return false
}

// HasAll reports whether granted contains every scope in want. An empty want is
// satisfied by any grant.
func HasAll(granted []string, want []string) bool {
	for _, w := range want {
		if !Has(granted, w) {
			return false
		}
	}
	return true
}

// Missing returns the entries of want that granted does not contain, so an
// error can name exactly what to ask for.
func Missing(granted []string, want []string) []string {
	var out []string
	for _, w := range want {
		if !Has(granted, w) {
			out = append(out, w)
		}
	}
	return out
}
