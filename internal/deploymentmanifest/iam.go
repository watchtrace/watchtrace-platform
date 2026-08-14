package deploymentmanifest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
)

type IAMAPI interface {
	GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	ListRolePolicies(context.Context, *iam.ListRolePoliciesInput, ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error)
	GetRolePolicy(context.Context, *iam.GetRolePolicyInput, ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error)
	ListAttachedRolePolicies(context.Context, *iam.ListAttachedRolePoliciesInput, ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error)
	GetPolicy(context.Context, *iam.GetPolicyInput, ...func(*iam.Options)) (*iam.GetPolicyOutput, error)
	GetPolicyVersion(context.Context, *iam.GetPolicyVersionInput, ...func(*iam.Options)) (*iam.GetPolicyVersionOutput, error)
}

type policyInventory struct {
	Kind     string          `json:"kind"`
	Name     string          `json:"name"`
	ARN      string          `json:"arn,omitempty"`
	Version  string          `json:"version,omitempty"`
	Document json.RawMessage `json:"document"`
}

func VerifyIAM(ctx context.Context, client IAMAPI, manifest Manifest) error {
	if client == nil {
		return errors.New("IAM client is required")
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	for key, expected := range manifest.Roles {
		role, err := client.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(expected.Name)})
		if err != nil || role.Role == nil || aws.ToString(role.Role.RoleName) != expected.Name {
			return fmt.Errorf("read IAM role: %s", key)
		}
		trust, err := fingerprintJSON(aws.ToString(role.Role.AssumeRolePolicyDocument))
		if err != nil || trust != expected.TrustFingerprintSHA256 {
			return fmt.Errorf("IAM trust drift: %s", key)
		}
		inventory, err := readRolePolicies(ctx, client, expected.Name)
		if err != nil {
			return fmt.Errorf("read IAM policies: %s: %w", key, err)
		}
		encoded, err := json.Marshal(inventory)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(encoded)
		if fmt.Sprintf("%x", sum) != expected.PolicyFingerprintSHA256 {
			return fmt.Errorf("IAM policy drift: %s", key)
		}
	}
	return nil
}

func readRolePolicies(ctx context.Context, client IAMAPI, roleName string) ([]policyInventory, error) {
	var inlineNames []string
	var inlineMarker *string
	for {
		inline, err := client.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: aws.String(roleName), Marker: inlineMarker})
		if err != nil {
			return nil, err
		}
		inlineNames = append(inlineNames, inline.PolicyNames...)
		if !inline.IsTruncated {
			break
		}
		if inline.Marker == nil || aws.ToString(inline.Marker) == "" {
			return nil, errors.New("IAM inline policy pagination omitted its marker")
		}
		inlineMarker = inline.Marker
	}
	var attachedPolicies []iamtypes.AttachedPolicy
	var attachedMarker *string
	for {
		attached, err := client.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(roleName), Marker: attachedMarker})
		if err != nil {
			return nil, err
		}
		attachedPolicies = append(attachedPolicies, attached.AttachedPolicies...)
		if !attached.IsTruncated {
			break
		}
		if attached.Marker == nil || aws.ToString(attached.Marker) == "" {
			return nil, errors.New("IAM attached policy pagination omitted its marker")
		}
		attachedMarker = attached.Marker
	}
	result := make([]policyInventory, 0, len(inlineNames)+len(attachedPolicies))
	for _, name := range inlineNames {
		policy, policyErr := client.GetRolePolicy(ctx, &iam.GetRolePolicyInput{RoleName: aws.String(roleName), PolicyName: aws.String(name)})
		if policyErr != nil {
			return nil, policyErr
		}
		document, policyErr := canonicalPolicy(aws.ToString(policy.PolicyDocument))
		if policyErr != nil {
			return nil, policyErr
		}
		result = append(result, policyInventory{Kind: "inline", Name: name, Document: document})
	}
	for _, attachedPolicy := range attachedPolicies {
		arn := aws.ToString(attachedPolicy.PolicyArn)
		policy, policyErr := client.GetPolicy(ctx, &iam.GetPolicyInput{PolicyArn: aws.String(arn)})
		if policyErr != nil {
			return nil, policyErr
		}
		if policy.Policy == nil {
			return nil, errors.New("IAM attached policy response omitted the policy")
		}
		version := aws.ToString(policy.Policy.DefaultVersionId)
		versionOutput, policyErr := client.GetPolicyVersion(ctx, &iam.GetPolicyVersionInput{PolicyArn: aws.String(arn), VersionId: aws.String(version)})
		if policyErr != nil {
			return nil, policyErr
		}
		if versionOutput.PolicyVersion == nil {
			return nil, errors.New("IAM policy-version response omitted the policy version")
		}
		document, policyErr := canonicalPolicy(aws.ToString(versionOutput.PolicyVersion.Document))
		if policyErr != nil {
			return nil, policyErr
		}
		result = append(result, policyInventory{Kind: "attached", Name: aws.ToString(attachedPolicy.PolicyName), ARN: arn, Version: version, Document: document})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Kind != result[j].Kind {
			return result[i].Kind < result[j].Kind
		}
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].ARN < result[j].ARN
	})
	return result, nil
}

func canonicalPolicy(value string) (json.RawMessage, error) {
	decoded, err := url.QueryUnescape(value)
	if err != nil {
		return nil, err
	}
	var document any
	if err = json.Unmarshal([]byte(decoded), &document); err != nil {
		return nil, err
	}
	return json.Marshal(document)
}
