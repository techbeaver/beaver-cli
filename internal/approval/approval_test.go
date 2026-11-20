package approval

import "testing"

func TestClassifyEveryStatus(t *testing.T) {
	cases := []struct {
		status  string
		want    Outcome
		settled bool
	}{
		{"approved", Approved, true},
		{"pending", Pending, false},
		{"rejected", Rejected, true},
		{"declined", Rejected, true},
		{"consumed", Consumed, true},
		{"expired", Expired, true},
		{"", Unknown, true},
		{"cancelled", Unknown, true},
		{"APPROVED", Unknown, true},
		{" approved", Unknown, true},
		{"approved ", Unknown, true},
		{"approve", Unknown, true},
	}
	for _, c := range cases {
		got := Classify(c.status)
		if got != c.want {
			t.Errorf("Classify(%q) = %v, want %v", c.status, got, c.want)
		}
		if got.Settled() != c.settled {
			t.Errorf("Classify(%q).Settled() = %v, want %v", c.status, got.Settled(), c.settled)
		}
	}
}

func TestOnlyTheApprovedStatusIsApproval(t *testing.T) {
	var zero Outcome
	if zero == Approved {
		t.Fatal("the zero Outcome must not be Approved")
	}
	for _, s := range []string{"rejected", "declined", "consumed", "expired", "pending", "", "x", "Approved"} {
		if Classify(s) == Approved {
			t.Errorf("%q was classified as approved", s)
		}
	}
}
