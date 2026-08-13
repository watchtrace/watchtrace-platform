package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/watchtrace/watchtrace-platform/internal/deploymentmanifest"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: queue-admin MANIFEST.json")
		os.Exit(2)
	}
	manifest, err := deploymentmanifest.Load(os.Args[1])
	if err != nil {
		fail()
	}
	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(manifest.AWSRegion))
	if err != nil {
		fail()
	}
	client := sqs.NewFromConfig(cfg, func(options *sqs.Options) {
		if endpoint := strings.TrimSpace(os.Getenv("WATCHTRACE_SQS_ENDPOINT")); endpoint != "" {
			options.BaseEndpoint = aws.String(endpoint)
		}
	})
	if deploymentmanifest.VerifySQS(ctx, client, manifest) != nil {
		fail()
	}
	fmt.Println("deployment manifest verified")
}

func fail() { fmt.Fprintln(os.Stderr, "deployment manifest verification failed"); os.Exit(1) }
