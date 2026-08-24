package reliability

import (
	"testing"
	"time"
)

func TestReportNormalizationKeepsMissingUnknown(t *testing.T) {
	r := (Report{Expected: 60, Observed: 10, Successful: 9, Unknown: 50}).Normalize()
	if r.ObservedUptime == nil || *r.ObservedUptime != .9 || r.Coverage == nil || *r.Coverage != float64(10)/60 {
		t.Fatalf("report=%+v", r)
	}
}
func TestReportWithNoObservedHasNoUptime(t *testing.T) {
	r := (Report{Expected: 5, Unknown: 5}).Normalize()
	if r.ObservedUptime != nil || r.Coverage == nil || *r.Coverage != 0 {
		t.Fatalf("report=%+v", r)
	}
}

func TestReportWithNoExpectedHasNoRatios(t *testing.T) {
	r := (Report{}).Normalize()
	if r.ObservedUptime != nil || r.Coverage != nil || r.Unknown != 0 {
		t.Fatalf("report=%+v", r)
	}
}

func TestObservedStateThresholdsAndRecovery(t *testing.T) {
	state, failures, successes := "unknown", 0, 0
	for _, succeeded := range []bool{false, false, false, true, true} {
		state, failures, successes = advanceObservedState(state, failures, successes, succeeded)
	}
	if state != "healthy" || failures != 0 || successes != 0 {
		t.Fatalf("state=%s failures=%d successes=%d", state, failures, successes)
	}
}

func TestOrderUsesScheduledTimeThenJobID(t *testing.T) {
	at := time.Unix(100, 0).UTC()
	if !orderAfter(at.Add(time.Second), "a", at, "z") || !orderAfter(at, "b", at, "a") || orderAfter(at, "a", at, "a") {
		t.Fatal("unexpected slot ordering")
	}
}
