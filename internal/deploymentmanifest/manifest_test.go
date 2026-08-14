package deploymentmanifest

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
)

type fakeIAM struct {
	trust, policy string
}

func (f fakeIAM) GetRole(_ context.Context, input *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	return &iam.GetRoleOutput{Role: &types.Role{RoleName: input.RoleName, AssumeRolePolicyDocument: aws.String(f.trust)}}, nil
}
func (f fakeIAM) ListRolePolicies(context.Context, *iam.ListRolePoliciesInput, ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error) {
	return &iam.ListRolePoliciesOutput{PolicyNames: []string{"queue-access"}}, nil
}
func (f fakeIAM) GetRolePolicy(context.Context, *iam.GetRolePolicyInput, ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error) {
	return &iam.GetRolePolicyOutput{PolicyDocument: aws.String(f.policy)}, nil
}
func (f fakeIAM) ListAttachedRolePolicies(context.Context, *iam.ListAttachedRolePoliciesInput, ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	return &iam.ListAttachedRolePoliciesOutput{}, nil
}
func (f fakeIAM) GetPolicy(context.Context, *iam.GetPolicyInput, ...func(*iam.Options)) (*iam.GetPolicyOutput, error) {
	return &iam.GetPolicyOutput{}, nil
}
func (f fakeIAM) GetPolicyVersion(context.Context, *iam.GetPolicyVersionInput, ...func(*iam.Options)) (*iam.GetPolicyVersionOutput, error) {
	return &iam.GetPolicyVersionOutput{}, nil
}

func TestManifestRequiresAuthoritativeQueueAndRoleInventory(t *testing.T) {
	manifest := validManifest(t)
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	manifest.Queues["jobs"] = Queue{Name: "watchtrace-dev-jobs.fifo"}
	if err := manifest.Validate(); err == nil {
		t.Fatal("obsolete queue name was accepted")
	}
}

func TestVerifyIAMFingerprintsTrustAndEffectivePolicies(t *testing.T) {
	trust := `{"Version":"2012-10-17","Statement":[]}`
	policy := `{"Version":"2012-10-17","Statement":[]}`
	manifest := validManifest(t)
	trustFingerprint, err := fingerprintJSON(trust)
	if err != nil {
		t.Fatal(err)
	}
	inventory := `[ {"kind":"inline","name":"queue-access","document":{"Statement":[],"Version":"2012-10-17"}} ]`
	inventory = strings.ReplaceAll(inventory, " ", "")
	policySum := sha256.Sum256([]byte(inventory))
	for key, role := range manifest.Roles {
		role.TrustFingerprintSHA256 = trustFingerprint
		role.PolicyFingerprintSHA256 = fmt.Sprintf("%x", policySum)
		manifest.Roles[key] = role
	}
	if err = VerifyIAM(context.Background(), fakeIAM{trust: trust, policy: policy}, manifest); err != nil {
		t.Fatal(err)
	}
	role := manifest.Roles["job_publisher"]
	role.TrustFingerprintSHA256 = strings.Repeat("0", 64)
	manifest.Roles["job_publisher"] = role
	if err = VerifyIAM(context.Background(), fakeIAM{trust: trust, policy: policy}, manifest); err == nil {
		t.Fatal("trust-policy drift was accepted")
	}
}

func validManifest(t *testing.T) Manifest {
	t.Helper()
	prefix := "watchtrace-dev-"
	account := "123456789012"
	region := "eu-north-1"
	queue := func(name string) (string, string) {
		return "https://sqs." + region + ".amazonaws.com/" + account + "/" + name, "arn:aws:sqs:" + region + ":" + account + ":" + name
	}
	jobURL, jobARN := queue(prefix + "check-jobs-hosted.fifo")
	resultURL, resultARN := queue(prefix + "check-results.fifo")
	jobDLQURL, jobDLQARN := queue(prefix + "check-jobs-hosted-dlq.fifo")
	resultDLQURL, resultDLQARN := queue(prefix + "check-results-dlq.fifo")
	fingerprint := strings.Repeat("a", 64)
	roleKeys := map[string]string{"job_publisher": "job-publisher", "hosted_worker": "hosted-worker", "queue_gateway": "queue-gateway", "result_consumer": "result-consumer", "dlq_reconciler": "dlq-reconciler", "infrastructure_operator": "infrastructure-operator"}
	roles := map[string]Role{}
	for key, suffix := range roleKeys {
		roles[key] = Role{Name: prefix + suffix, PolicyFingerprintSHA256: fingerprint, TrustFingerprintSHA256: fingerprint}
	}
	return Manifest{Version: 1, Environment: "dev", AWSRegion: region, Queues: map[string]Queue{
		"jobs":        {Name: prefix + "check-jobs-hosted.fifo", URL: jobURL, ARN: jobARN, VisibilityTimeoutSeconds: 90, MessageRetentionSeconds: 345600, ReceiveWaitTimeSeconds: 20, MaxReceiveCount: 5, DeadLetterQueueARN: jobDLQARN, SSE: "SSE-SQS", PolicyFingerprintSHA256: fingerprint},
		"results":     {Name: prefix + "check-results.fifo", URL: resultURL, ARN: resultARN, VisibilityTimeoutSeconds: 60, MessageRetentionSeconds: 345600, ReceiveWaitTimeSeconds: 20, MaxReceiveCount: 10, DeadLetterQueueARN: resultDLQARN, SSE: "SSE-SQS", PolicyFingerprintSHA256: fingerprint},
		"jobs_dlq":    {Name: prefix + "check-jobs-hosted-dlq.fifo", URL: jobDLQURL, ARN: jobDLQARN, MessageRetentionSeconds: 1209600, SSE: "SSE-SQS", PolicyFingerprintSHA256: fingerprint, RedriveAllowSourceARNs: []string{jobARN}},
		"results_dlq": {Name: prefix + "check-results-dlq.fifo", URL: resultDLQURL, ARN: resultDLQARN, MessageRetentionSeconds: 1209600, SSE: "SSE-SQS", PolicyFingerprintSHA256: fingerprint, RedriveAllowSourceARNs: []string{resultARN}},
	}, Roles: roles, Tags: map[string]string{"Application": "WatchTrace", "Environment": "dev", "Phase": "1"}}
}
