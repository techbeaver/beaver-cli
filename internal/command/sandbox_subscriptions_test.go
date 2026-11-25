package command

import (
	"strings"
	"testing"
)

func subWith(extra string) string {
	return `{"id":"` + testSubID + `","projectId":"` + testProject + `","planId":"` + testPlan + `","planName":"Small","status":"active","slots":2,"slotsInUse":1,` +
		`"currency":"NGN","unitMonthlyMinor":150000,"hourlyMinor":500,"nextChargeMinor":0,"nextChargeSlots":0,` + extra + `}`
}

func showSub(t *testing.T, raw string) string {
	t.Helper()
	api := newSBServer(t, fixedSB(raw))
	out, _, err := sbRun(t, api, "sandbox", "subscriptions", testSubID, "--format", "table")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSubscriptionShowsTheAutoPayFieldsTheAPISent(t *testing.T) {
	monthly := showSub(t, subWith(`"billingMode":"monthly","autoRenew":true`))
	if !strings.Contains(monthly, "autoRenew") || !strings.Contains(monthly, "on") || strings.Contains(monthly, "hourlyAutoTopUp") {
		t.Fatalf("a monthly subscription shows autoRenew only:\n%s", monthly)
	}
	off := showSub(t, subWith(`"billingMode":"monthly","autoRenew":false`))
	if !strings.Contains(off, "off") {
		t.Fatalf("autoRenew false is off, not missing:\n%s", off)
	}
	hourly := showSub(t, subWith(`"billingMode":"hourly","hourlyAutoTopUp":true,"hourlyTopUp":{"amountMinor":1000000,"currency":"NGN","max":3,"windowHours":24}`))
	for _, want := range []string{"hourlyAutoTopUp", "on", "hourlyTopUp", "NGN 10,000.00 per top-up, at most 3 in 24 hours"} {
		if !strings.Contains(hourly, want) {
			t.Errorf("hourly view lacks %q:\n%s", want, hourly)
		}
	}
	if strings.Contains(hourly, "autoRenew") {
		t.Fatalf("an hourly subscription has no renewal switch:\n%s", hourly)
	}
}

func TestSubscriptionAutoPayFieldsNeverInventAValue(t *testing.T) {
	absent := showSub(t, subWith(`"billingMode":"hourly"`))
	for _, name := range []string{"autoRenew", "hourlyAutoTopUp", "hourlyTopUp"} {
		if strings.Contains(absent, name) {
			t.Fatalf("%s was not sent, so it must not be shown (a missing switch is not off):\n%s", name, absent)
		}
	}
	for name, extra := range map[string]string{
		"not a bool":      `"autoRenew":"yes","hourlyAutoTopUp":1`,
		"rule is a list":  `"hourlyTopUp":[1]`,
		"rule is empty":   `"hourlyTopUp":{}`,
		"rule is null":    `"hourlyTopUp":null,"autoRenew":null`,
		"rule has no cap": `"hourlyTopUp":{"amountMinor":500,"currency":"USD"}`,
	} {
		out := showSub(t, subWith(`"billingMode":"hourly",`+extra))
		if strings.Contains(out, "on") && name == "not a bool" && strings.Contains(out, "autoRenew") {
			t.Fatalf("%s: a non-boolean switch was displayed:\n%s", name, out)
		}
		if name == "rule has no cap" && (!strings.Contains(out, "USD 5.00 per top-up") || strings.Contains(out, "at most")) {
			t.Fatalf("%s: states only what the API gave:\n%s", name, out)
		}
	}
}

func TestSubscriptionListColumnsCarryTheSwitches(t *testing.T) {
	api := newSBServer(t, fixedSB(`[`+subWith(`"billingMode":"monthly","autoRenew":true`)+`,`+subWith(`"billingMode":"hourly","hourlyAutoTopUp":false`)+`]`))
	out, _, err := sbRun(t, api, "sandbox", "subscriptions", "--project", testProject, "--format", "table")
	if err != nil {
		t.Fatal(err)
	}
	if up := strings.ToUpper(out); !strings.Contains(up, "AUTORENEW") || !strings.Contains(up, "HOURLYAUTOTOPUP") {
		t.Fatalf("both switches have a column:\n%s", out)
	}
}

func TestTopUpRuleTextBoundaries(t *testing.T) {
	for name, tc := range map[string]struct {
		rule map[string]any
		want string
	}{
		"empty":       {map[string]any{}, ""},
		"zero max":    {map[string]any{"amountMinor": int64(0), "currency": "USD", "max": int64(0), "windowHours": int64(0)}, "USD 0.00 per top-up, at most 0 in 0 hours"},
		"no currency": {map[string]any{"amountMinor": int64(100)}, "100 (minor units, currency not stated) per top-up"},
		"no window":   {map[string]any{"amountMinor": int64(100), "currency": "USD", "max": int64(2)}, "USD 1.00 per top-up, at most 2"},
	} {
		if got := topUpRuleText(tc.rule); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}
