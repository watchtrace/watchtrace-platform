package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/watchtrace/watchtrace-platform/internal/deploymentmanifest"
)

func main() {
	scope, manifestPath, err := parseArguments(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "usage: queue-admin [-scope sqs|all] MANIFEST.json")
		os.Exit(2)
	}
	manifest, err := deploymentmanifest.Load(manifestPath)
	if err != nil {
		fail("load deployment manifest", err)
	}
	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(manifest.AWSRegion))
	if err != nil {
		fail("load AWS configuration", err)
	}
	client := sqs.NewFromConfig(cfg, func(options *sqs.Options) {
		if endpoint := strings.TrimSpace(os.Getenv("WATCHTRACE_SQS_ENDPOINT")); endpoint != "" {
			options.BaseEndpoint = aws.String(endpoint)
		}
	})
	if err = deploymentmanifest.VerifySQS(ctx, client, manifest); err != nil {
		fail("verify SQS resources", err)
	}
	if scope == "all" {
		if err = deploymentmanifest.VerifyIAM(ctx, iam.NewFromConfig(cfg), manifest); err != nil {
			fail("verify IAM resources", err)
		}
	}
	fmt.Println("deployment manifest verified")
}

func parseArguments(arguments []string) (string, string, error) {
	flags := flag.NewFlagSet("queue-admin", flag.ContinueOnError)
	flags.SetOutput(new(strings.Builder))
	scope := flags.String("scope", "all", "verification scope: sqs or all")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 1 || (*scope != "sqs" && *scope != "all") {
		return "", "", fmt.Errorf("invalid arguments")
	}
	return *scope, flags.Arg(0), nil
}

func fail(operation string, err error) {
	fmt.Fprintf(os.Stderr, "deployment manifest verification failed: %s: %v\n", operation, err)
	os.Exit(1)
}
