package scopes

import (
	"strings"
	"testing"
)

func TestThereAreNoAdminScopes(t *testing.T) {
	for _, s := range All {
		lower := strings.ToLower(s)
		if strings.Contains(lower, "admin") || strings.Contains(lower, "support") ||
			strings.Contains(lower, "payout") || strings.Contains(lower, "refund") {
			t.Fatalf("scope %q looks administrative; machine credentials must never have one", s)
		}
	}
}

func TestDefaultGrantCannotDestroy(t *testing.T) {
	for _, s := range Default {
		if IsDestructive(s) {
			t.Fatalf("%q is destructive and must not be in the default grant", s)
		}
	}
	if Has(Default, BillingSpend) {
		t.Fatal("billing:spend must never be in the default grant")
	}
}

func TestBillingSpendIsDefinedButNotIssuable(t *testing.T) {
	if !IsKnown(BillingSpend) {
		t.Fatal("billing:spend must stay in the vocabulary so enabling it later is a UI change")
	}
	if Issuable(BillingSpend) {
		t.Fatal("billing:spend must not be issuable in v1")
	}
	for _, s := range Issuables() {
		if s == BillingSpend {
			t.Fatal("billing:spend leaked into the issuable list")
		}
	}
}

func TestEveryScopeHasConsentWording(t *testing.T) {
	for _, s := range All {
		if Describe(s) == s {
			t.Fatalf("scope %q has no consent-screen wording; a customer would be shown a raw identifier", s)
		}
	}
}

func TestDescriptionsQuoteNoPricesOrPlanNames(t *testing.T) {
	banned := []string{"NGN", "₦", "$", "free", "hobby", "pro plan", "%"}
	for _, s := range All {
		d := strings.ToLower(Describe(s))
		for _, b := range banned {
			if strings.Contains(d, strings.ToLower(b)) {
				t.Fatalf("consent wording for %q mentions %q; prices and plan names must come from the API", s, b)
			}
		}
	}
}

func TestParseReportsUnknownScopesRatherThanDroppingThem(t *testing.T) {
	known, unknown := Parse("paas:read admin:everything dbaas:read")
	if len(known) != 2 || known[0] != DBaaSRead || known[1] != PaaSRead {
		t.Fatalf("unexpected known scopes: %v", known)
	}
	if len(unknown) != 1 || unknown[0] != "admin:everything" {
		t.Fatalf("an unrecognised scope must be reported, got %v", unknown)
	}
}

func TestParseAcceptsCommasAndDeduplicates(t *testing.T) {
	known, _ := Parse("paas:read, paas:read,paas:write")
	if len(known) != 2 {
		t.Fatalf("expected two distinct scopes, got %v", known)
	}
}

func TestMissingNamesWhatToAskFor(t *testing.T) {
	missing := Missing([]string{PaaSRead}, []string{PaaSRead, PaaSDestroy})
	if len(missing) != 1 || missing[0] != PaaSDestroy {
		t.Fatalf("Missing must name exactly what is absent, got %v", missing)
	}
	if !HasAll([]string{PaaSRead}, nil) {
		t.Fatal("an empty requirement is satisfied by any grant")
	}
}
