// Package deploymentmanifest validates the non-secret Phase 1 AWS inventory.
package deploymentmanifest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

const Version = 1

var (
	environmentPattern = regexp.MustCompile(`^(dev|stg|prod)$`)
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
	Name                      string   `json:"name"`
	URL                       string   `json:"url"`
	ARN                       string   `json:"arn"`
	VisibilityTimeoutSeconds  int      `json:"visibility_timeout_seconds"`
	MessageRetentionSeconds   int      `json:"message_retention_seconds"`
	ReceiveWaitTimeSeconds    int      `json:"receive_wait_time_seconds"`
	MaxReceiveCount           int      `json:"max_receive_count,omitempty"`
	DeadLetterQueueARN        string   `json:"dead_letter_queue_arn,omitempty"`
	SSE                       string   `json:"sse"`
	ContentBasedDeduplication bool     `json:"content_based_deduplication"`
	PolicyFingerprintSHA256   string   `json:"policy_fingerprint_sha256"`
	RedriveAllowSourceARNs    []string `json:"redrive_allow_source_arns,omitempty"`
}

type Role struct {
	Name                    string `json:"name"`
	PolicyFingerprintSHA256 string `json:"policy_fingerprint_sha256"`
	TrustFingerprintSHA256  string `json:"trust_fingerprint_sha256"`
}

type SQSAPI interface {
	GetQueueUrl(context.Context, *sqs.GetQueueUrlInput, ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error)
	GetQueueAttributes(context.Context, *sqs.GetQueueAttributesInput, ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
	ListQueueTags(context.Context, *sqs.ListQueueTagsInput, ...func(*sqs.Options)) (*sqs.ListQueueTagsOutput, error)
	ListDeadLetterSourceQueues(context.Context, *sqs.ListDeadLetterSourceQueuesInput, ...func(*sqs.Options)) (*sqs.ListDeadLetterSourceQueuesOutput, error)
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
		"jobs": {"check-jobs-hosted.fifo", 90, 345600, 20, 5}, "results": {"check-results.fifo", 60, 345600, 20, 10},
		"jobs_dlq": {"check-jobs-hosted-dlq.fifo", 0, 1209600, 0, 0}, "results_dlq": {"check-results-dlq.fifo", 0, 1209600, 0, 0},
	}
	accounts := map[string]struct{}{}
	for key, want := range expected {
		queue, ok := m.Queues[key]
		if !ok || queue.Name != "watchtrace-"+m.Environment+"-"+want.suffix || queue.URL == "" || queue.ARN == "" || queue.VisibilityTimeoutSeconds != want.visibility || queue.MessageRetentionSeconds != want.retention || queue.ReceiveWaitTimeSeconds != want.wait || queue.SSE != "SSE-SQS" || queue.ContentBasedDeduplication || queue.MaxReceiveCount != want.receives || !fingerprintPattern.MatchString(queue.PolicyFingerprintSHA256) {
			return fmt.Errorf("invalid queue manifest: %s", key)
		}
		parsedURL, err := url.Parse(queue.URL)
		parts := strings.Split(queue.ARN, ":")
		if err != nil || parsedURL.Scheme == "" || path.Base(parsedURL.Path) != queue.Name || len(parts) != 6 || parts[0] != "arn" || parts[2] != "sqs" || parts[3] != m.AWSRegion || parts[4] == "" || parts[5] != queue.Name {
			return fmt.Errorf("invalid queue identity: %s", key)
		}
		accounts[parts[4]] = struct{}{}
		if want.receives > 0 && queue.DeadLetterQueueARN == "" {
			return fmt.Errorf("missing redrive target: %s", key)
		}
		if want.receives == 0 && len(queue.RedriveAllowSourceARNs) != 1 {
			return fmt.Errorf("invalid redrive allow sources: %s", key)
		}
	}
	if len(accounts) != 1 || m.Queues["jobs"].DeadLetterQueueARN != m.Queues["jobs_dlq"].ARN || m.Queues["results"].DeadLetterQueueARN != m.Queues["results_dlq"].ARN || m.Queues["jobs_dlq"].RedriveAllowSourceARNs[0] != m.Queues["jobs"].ARN || m.Queues["results_dlq"].RedriveAllowSourceARNs[0] != m.Queues["results"].ARN {
		return errors.New("queue routing inventory is inconsistent")
	}
	for key, value := range map[string]string{"Application": "WatchTrace", "Environment": m.Environment, "Phase": "1"} {
		if m.Tags[key] != value {
			return fmt.Errorf("invalid deployment tag: %s", key)
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
		policyFingerprint, err := fingerprintJSON(out.Attributes[string(types.QueueAttributeNamePolicy)])
		if err != nil || policyFingerprint != queue.PolicyFingerprintSHA256 {
			return fmt.Errorf("queue policy drift: %s", key)
		}
		if queue.MaxReceiveCount > 0 {
			var redrive struct {
				DeadLetterTargetARN string          `json:"deadLetterTargetArn"`
				MaxReceiveCount     json.RawMessage `json:"maxReceiveCount"`
			}
			if json.Unmarshal([]byte(out.Attributes[string(types.QueueAttributeNameRedrivePolicy)]), &redrive) != nil || redrive.DeadLetterTargetARN != queue.DeadLetterQueueARN || strings.Trim(string(redrive.MaxReceiveCount), `"`) != strconv.Itoa(queue.MaxReceiveCount) {
				return fmt.Errorf("queue redrive drift: %s", key)
			}
		} else {
			var allow struct {
				RedrivePermission string   `json:"redrivePermission"`
				SourceQueueARNs   []string `json:"sourceQueueArns"`
			}
			if json.Unmarshal([]byte(out.Attributes[string(types.QueueAttributeNameRedriveAllowPolicy)]), &allow) != nil || allow.RedrivePermission != "byQueue" || !sameStrings(allow.SourceQueueARNs, queue.RedriveAllowSourceARNs) {
				return fmt.Errorf("queue redrive allow drift: %s", key)
			}
			sources, sourceErr := client.ListDeadLetterSourceQueues(ctx, &sqs.ListDeadLetterSourceQueuesInput{QueueUrl: url.QueueUrl})
			if sourceErr != nil || len(sources.QueueUrls) != 1 || path.Base(sources.QueueUrls[0]) != arnResource(queue.RedriveAllowSourceARNs[0]) {
				return fmt.Errorf("queue redrive source drift: %s", key)
			}
		}
		tags, err := client.ListQueueTags(ctx, &sqs.ListQueueTagsInput{QueueUrl: url.QueueUrl})
		if err != nil {
			return fmt.Errorf("read queue tags: %s: %w", key, err)
		}
		for tag, want := range manifest.Tags {
			if tags.Tags[tag] != want {
				return fmt.Errorf("queue tag drift: %s/%s", key, tag)
			}
		}
	}
	return nil
}

func arnResource(value string) string {
	parts := strings.Split(value, ":")
	if len(parts) != 6 {
		return ""
	}
	return parts[5]
}

func fingerprintJSON(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("missing JSON policy")
	}
	decoded, err := url.QueryUnescape(value)
	if err != nil {
		return "", err
	}
	var document any
	if err = json.Unmarshal([]byte(decoded), &document); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return fmt.Sprintf("%x", sum), nil
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
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
