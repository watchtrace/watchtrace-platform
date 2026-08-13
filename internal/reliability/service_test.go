package reliability

import "testing"

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
