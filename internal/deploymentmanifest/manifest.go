// Package deploymentmanifest validates the non-secret Phase 1 AWS inventory.
package deploymentmanifest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

const Version = 1

var (
	environmentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
	fingerprintPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type Manifest struct {
	Version     int               `json:"version"`
	Environment string            `json:"environment"`
	AWSRegion   string            `json:"aws_region"`
	Queues      map[string]Queue  `json:"queues"`
	Roles       map[string]Role   `json:"roles"`
	Gateway     map[string]string `json:"gateway_mappings,omitempty"`
	Tags        map[string]string `json:"tags"`
}

type Queue struct {
	Name                      string `json:"name"`
	URL                       string `json:"url"`
	ARN                       string `json:"arn"`
	VisibilityTimeoutSeconds  int    `json:"visibility_timeout_seconds"`
	MessageRetentionSeconds   int    `json:"message_retention_seconds"`
	ReceiveWaitTimeSeconds    int    `json:"receive_wait_time_seconds"`
	MaxReceiveCount           int    `json:"max_receive_count,omitempty"`
	DeadLetterQueueARN        string `json:"dead_letter_queue_arn,omitempty"`
	SSE                       string `json:"sse"`
	ContentBasedDeduplication bool   `json:"content_based_deduplication"`
}

type Role struct {
	Name                    string `json:"name"`
	PolicyFingerprintSHA256 string `json:"policy_fingerprint_sha256"`
	TrustFingerprintSHA256  string `json:"trust_fingerprint_sha256"`
}

type SQSAPI interface {
	GetQueueUrl(context.Context, *sqs.GetQueueUrlInput, ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error)
	GetQueueAttributes(context.Context, *sqs.GetQueueAttributesInput, ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
}

func Load(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	decoder := json.NewDecoder(newBoundedReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode deployment manifest: %w", err)
	}
	return manifest, manifest.Validate()
}

func (m Manifest) Validate() error {
	if m.Version != Version || !environmentPattern.MatchString(m.Environment) || m.AWSRegion == "" || len(m.Queues) != 4 {
		return errors.New("invalid deployment manifest identity")
	}
	expected := map[string]struct {
		suffix                                string
		visibility, retention, wait, receives int
	}{
		"jobs": {"jobs.fifo", 90, 345600, 20, 5}, "results": {"results.fifo", 60, 345600, 20, 10},
		"jobs_dlq": {"jobs-dlq.fifo", 0, 1209600, 0, 0}, "results_dlq": {"results-dlq.fifo", 0, 1209600, 0, 0},
	}
	for key, want := range expected {
		queue, ok := m.Queues[key]
		if !ok || queue.Name != "watchtrace-"+m.Environment+"-"+want.suffix || queue.URL == "" || queue.ARN == "" || queue.VisibilityTimeoutSeconds != want.visibility || queue.MessageRetentionSeconds != want.retention || queue.ReceiveWaitTimeSeconds != want.wait || queue.SSE != "SSE-SQS" || queue.ContentBasedDeduplication || queue.MaxReceiveCount != want.receives {
			return fmt.Errorf("invalid queue manifest: %s", key)
		}
		if want.receives > 0 && queue.DeadLetterQueueARN == "" {
			return fmt.Errorf("missing redrive target: %s", key)
		}
	}
	requiredRoles := []string{"job_publisher", "hosted_worker", "queue_gateway", "result_consumer", "dlq_reconciler", "infrastructure_operator"}
	for _, key := range requiredRoles {
		role, ok := m.Roles[key]
		if !ok || role.Name == "" || !fingerprintPattern.MatchString(role.PolicyFingerprintSHA256) || !fingerprintPattern.MatchString(role.TrustFingerprintSHA256) {
			return fmt.Errorf("invalid role fingerprint: %s", key)
		}
	}
	return nil
}

func VerifySQS(ctx context.Context, client SQSAPI, manifest Manifest) error {
	if client == nil {
		return errors.New("SQS client is required")
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	for key, queue := range manifest.Queues {
		url, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(queue.Name)})
		if err != nil || aws.ToString(url.QueueUrl) != queue.URL {
			return fmt.Errorf("queue URL drift: %s", key)
		}
		out, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: url.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameAll}})
		if err != nil {
			return fmt.Errorf("read queue attributes: %s: %w", key, err)
		}
		checks := map[types.QueueAttributeName]string{
			types.QueueAttributeNameQueueArn:                      queue.ARN,
			types.QueueAttributeNameFifoQueue:                     "true",
			types.QueueAttributeNameContentBasedDeduplication:     strconv.FormatBool(queue.ContentBasedDeduplication),
			types.QueueAttributeNameVisibilityTimeout:             strconv.Itoa(queue.VisibilityTimeoutSeconds),
			types.QueueAttributeNameMessageRetentionPeriod:        strconv.Itoa(queue.MessageRetentionSeconds),
			types.QueueAttributeNameReceiveMessageWaitTimeSeconds: strconv.Itoa(queue.ReceiveWaitTimeSeconds),
			types.QueueAttributeNameSqsManagedSseEnabled:          "true",
		}
		for attribute, want := range checks {
			if out.Attributes[string(attribute)] != want {
				return fmt.Errorf("queue attribute drift: %s/%s", key, attribute)
			}
		}
		if queue.MaxReceiveCount > 0 {
			var redrive struct {
				DeadLetterTargetARN string          `json:"deadLetterTargetArn"`
				MaxReceiveCount     json.RawMessage `json:"maxReceiveCount"`
			}
			if json.Unmarshal([]byte(out.Attributes[string(types.QueueAttributeNameRedrivePolicy)]), &redrive) != nil || redrive.DeadLetterTargetARN != queue.DeadLetterQueueARN || strings.Trim(string(redrive.MaxReceiveCount), `"`) != strconv.Itoa(queue.MaxReceiveCount) {
				return fmt.Errorf("queue redrive drift: %s", key)
			}
		}
	}
	return nil
}

type boundedReader struct {
	data   []byte
	offset int
}

func newBoundedReader(data []byte) *boundedReader { return &boundedReader{data: data} }
func (r *boundedReader) Read(p []byte) (int, error) {
	if len(r.data) > 1024*1024 {
		return 0, errors.New("manifest too large")
	}
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}
