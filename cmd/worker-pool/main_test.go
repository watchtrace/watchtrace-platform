package main

import "testing"

func TestSQSQueueARN(t *testing.T) {
	arn, region, account, err := sqsQueueARN("https://sqs.eu-north-1.amazonaws.com/825450315823/watchtrace-prod-check-jobs-hosted.fifo")
	if err != nil {
		t.Fatal(err)
	}
	if arn != "arn:aws:sqs:eu-north-1:825450315823:watchtrace-prod-check-jobs-hosted.fifo" || region != "eu-north-1" || account != "825450315823" {
		t.Fatalf("arn=%q region=%q account=%q", arn, region, account)
	}
}

func TestSQSQueueARNRejectsUnsafeOrNonFIFOValues(t *testing.T) {
	for _, value := range []string{
		"http://sqs.eu-north-1.amazonaws.com/825450315823/jobs.fifo",
		"https://sqs.eu-north-1.amazonaws.com/not-an-account/jobs.fifo",
		"https://sqs.eu-north-1.amazonaws.com/825450315823/jobs",
		"https://example.test/825450315823/jobs.fifo",
	} {
		if _, _, _, err := sqsQueueARN(value); err == nil {
			t.Fatalf("accepted invalid queue URL %q", value)
		}
	}
}
