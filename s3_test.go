package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/cobank-acb/cpe-terratest-helpers/src/helpers"
	"github.com/google/uuid"
	"github.com/gruntwork-io/terratest/modules/terraform"
	test_structure "github.com/gruntwork-io/terratest/modules/test-structure"
	"github.com/stretchr/testify/assert"
)

var (
	cfg      *aws.Config
	ctx      = context.Background()
	s3Client *s3.Client
)

const (
	awsAccountID = "546345899106"
	environment  = "dev"
	region       = "us-west-2"
	serviceName  = "cpe-container-orchestration-platform-3"
	argoRole     = "codebuild-actions-github-service-role"
)

// Setup necessary variables for the module
func accessTfVars() map[string]any {
	accessLogVars := map[string]any{
		"access_log_bucket":  nil,
		"aws_account_number": awsAccountID,
		"bucket_name":        fmt.Sprintf("%s-access-logs-%s-%s", serviceName, environment, region),
		"enable_bucket_acl":  true, // test with cloudfront
		"environment":        environment,
		"iam_role_id":        "",
		"name_prefix":        serviceName,
	}

	return accessLogVars
}

func terraformVars() map[string]any {
	testVars := map[string]any{
		"bucket_name":                  fmt.Sprintf("%s-terratest-%s-%s", awsAccountID, environment, region),
		"environment":                  environment,
		"force_destroy":                true,
		"is_argo_workflows_log_bucket": true,
		"name_prefix":                  serviceName,
		"argo_workflows_iam_role_name": argoRole,
	}

	return testVars
}

type PolicyStatement struct {
	Effect    string            `json:"Effect"`
	Action    []string          `json:"Action"`
	Resource  *string           `json:"Resource,omitempty"`
	Principal map[string]string `json:"Principal,omitempty"`
	Sid       string            `json:"Sid,omitempty"`
}

type PolicyDocument struct {
	Version   string            `json:"Version"`
	Statement []PolicyStatement `json:"Statement"`
}

// Initializes AWS Config and S3 client for testing
func init() {
	config, err := helpers.GetAwsConfig(region)
	if err != nil {
		panic(err)
	}

	// cfg = config
	cfg = config
	s3Client = s3.NewFromConfig(*cfg)
}

func TestCreateKmsKey(t *testing.T) {
	cfg, err := helpers.GetAwsConfig("us-west-2")
	if err != nil {
		t.Fatalf("failed to load AWS config: %v", err)
	}

	awsAccount := awsAccountID

	kmsKeyArn, err := helpers.CreateKmsKey(*cfg, awsAccount)
	if err != nil {
		t.Fatalf("failed to create KMS key: %v", err)
	}

	t.Logf("successfully created KMS key: %s", *kmsKeyArn)

	assert.NotNil(t, kmsKeyArn)

	helpers.DeleteKmsKey(t, *cfg, *kmsKeyArn)
}

func CreateCustomRole(cfg aws.Config, awsAccountID, kmsKeyArn string) (*string, error) {
	// Define the trust policy for the IAM role
	trustPolicy := PolicyDocument{
		Version: "2012-10-17",
		Statement: []PolicyStatement{
			{
				Effect: "Allow",
				Principal: map[string]string{
					"Service": "ec2.amazonaws.com",
				},
				Action: []string{"sts:AssumeRole"},
			},
		},
	}

	trustPolicyBytes, err := json.Marshal(trustPolicy)
	if err != nil {
		return nil, err
	}

	roleName := "test-role-" + uuid.New().String()
	createRoleInput := &iam.CreateRoleInput{
		RoleName:                 aws.String(roleName),
		AssumeRolePolicyDocument: aws.String(string(trustPolicyBytes)),
	}

	iamClient := iam.NewFromConfig(cfg)
	createRoleOutput, err := iamClient.CreateRole(context.TODO(), createRoleInput)
	if err != nil {
		return nil, err
	}

	kmsPolicy := PolicyDocument{
		Version: "2012-10-17",
		Statement: []PolicyStatement{
			{
				Effect: "Allow",
				Action: []string{
					"kms:Decrypt",
					"kms:Encrypt",
					"kms:GenerateDataKey",
					"kms:GenerateDataKeyWithoutPlaintext",
					"kms:ReEncryptTo",
					"kms:ReEncryptFrom",
				},
				Resource: aws.String(kmsKeyArn),
			},
		},
	}

	kmsPolicyBytes, err := json.Marshal(kmsPolicy)
	if err != nil {
		return nil, err
	}

	// Attach the policy to the IAM role
	putRolePolicyInput := &iam.PutRolePolicyInput{
		RoleName:       createRoleOutput.Role.RoleName,
		PolicyName:     aws.String("KmsPermissionsPolicy"),
		PolicyDocument: aws.String(string(kmsPolicyBytes)),
	}

	_, err = iamClient.PutRolePolicy(context.TODO(), putRolePolicyInput)
	if err != nil {
		return nil, err
	}

	s3Policy := PolicyDocument{
		Version: "2012-10-17",
		Statement: []PolicyStatement{
			{
				Effect: "Allow",
				Action: []string{
					"s3:PutObject",
					"s3:GetObject",
					"s3:ListBucket",
					"s3:DeleteObject",
					"s3:GetObjectVersion",
					"s3:DeleteObjectVersion",
					"s3:ListObjectsV2",
				},
				Resource: aws.String("*"),
			},
		},
	}

	s3PolicyBytes, err := json.Marshal(s3Policy)
	if err != nil {
		return nil, err
	}

	s3PutBucketPolicyInput := &s3.PutBucketPolicyInput{
		Bucket: aws.String("test-bucket"),
		Policy: aws.String(string(s3PolicyBytes)),
	}

	_, err = s3Client.PutBucketPolicy(context.TODO(), s3PutBucketPolicyInput)
	if err != nil {
		return nil, err
	}

	return createRoleOutput.Role.RoleName, nil
}

func getAssumeRoleClient(t *testing.T, roleName string, cfg aws.Config, bucketRegion string) *s3.Client {
	stsClient := sts.NewFromConfig(cfg)
	roleArn := fmt.Sprintf("arn:aws:iam::%s:role/%s", awsAccountID, roleName)
	roleSessionName := "S3CustomPolicyTest"

	creds := stscreds.NewAssumeRoleProvider(stsClient, roleArn, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = roleSessionName
	})

	assumeRoleCfg := aws.Config{
		Credentials: aws.NewCredentialsCache(creds),
		Region:      bucketRegion,
	}

	assumeS3Client := s3.NewFromConfig(assumeRoleCfg)

	return assumeS3Client
}

// Ensures the module can be applied
func TestApplyS3Module(t *testing.T) {
	// Make a copy of the terraform module to a temporary directory. This allows running multiple tests in parallel
	// against the same terraform module.
	tmpFolder := test_structure.CopyTerraformFolderToTemp(t, "../", "./")

	// Create the required KMS key and defer delete until
	// this test function completes
	keyArn, err := helpers.CreateKmsKey(*cfg, awsAccountID)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("successfully created test KMS key: %s", *keyArn)

	defer helpers.DeleteKmsKey(t, *cfg, *keyArn)

	// Create the required IAM role with KMS permissions and defer delete until
	// this test function completes
	roleName, err := CreateCustomRole(*cfg, awsAccountID, *keyArn)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("successfully created test IAM role: %s", *roleName)

	defer helpers.DeleteIamRole(t, *cfg, *roleName)

	// Create the required SNS topic and defer delete until
	// this test function completes
	topicArn, err := helpers.CreateSnsTopic(*cfg, false)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("successfully created test SNS topic: %s", *topicArn)

	defer helpers.DeleteSnsTopic(t, *cfg, *topicArn)

	// Create the required provider file when the test runs
	// since we don't want to persist providers in the module
	// as it will make count/for_each/depends_on at the module
	// level unusable
	if err := helpers.CreateAwsProviderFile(path.Join(tmpFolder, "providers.tf")); err != nil {
		t.Fatal(err)
	}

	// Setup terraform variables to create the required access log bucket
	accessLogVars := accessTfVars()
	accessLogVars["kms_master_key_id"] = *keyArn

	// Use a unique bucket name for each test run
	uniqueBucketName := fmt.Sprintf("%s-%s", accessLogVars["bucket_name"].(string), uuid.New().String())
	accessLogVars["bucket_name"] = uniqueBucketName

	terraformOptions := terraform.WithDefaultRetryableErrors(t, &terraform.Options{
		TerraformDir: tmpFolder,
		Vars:         accessLogVars,
	})

	// Create and set the workspace to use
	workspace := terraform.WorkspaceSelectOrNew(t, terraformOptions, "access-log-bucket")

	// Defer the destroy until after the function completes
	defer terraform.Destroy(t, terraformOptions)

	// Defer the workspace selection until after the function completes
	// to ensure the right state file is used for destroy operations
	defer terraform.WorkspaceSelectOrNew(t, terraformOptions, workspace)

	// Init and apply the terraform module
	ApplyOutput := terraform.InitAndApplyAndIdempotent(t, terraformOptions)

	// Get the number of resources that are created
	resourceCount := terraform.GetResourceCount(t, ApplyOutput)

	// Verify all resources were created
	assert.Equal(t, 8, resourceCount.Add)
	assert.Equal(t, 0, resourceCount.Change)
	assert.Equal(t, 0, resourceCount.Destroy)

	accessLogBucket := terraform.OutputMap(t, terraformOptions, "access_log_bucket")
	name := accessLogVars["bucket_name"].(string)
	if len(name) > 63 {
		name = name[:63]
	}
	assert.Equal(t, strings.TrimSuffix(name, "-"), accessLogBucket["id"])

	// Setup terraform variables and pass in required resources
	tfVars := terraformVars()
	tfVars["access_log_bucket"] = accessLogBucket
	tfVars["iam_role_id"] = *roleName
	tfVars["kms_master_key_id"] = *keyArn
	tfVars["sns_topic_arn"] = *topicArn

	// Use a unique bucket name for each test run
	uniqueBucketName = fmt.Sprintf("%s-%s", tfVars["bucket_name"].(string), uuid.New().String())
	tfVars["bucket_name"] = uniqueBucketName

	// Setup the working dir and terraform variables to be generated
	// at run time
	terraformOptions = terraform.WithDefaultRetryableErrors(t, &terraform.Options{
		TerraformDir: tmpFolder,
		Vars:         tfVars,
	})

	workspace = terraform.WorkspaceSelectOrNew(t, terraformOptions, "s3-buckets")

	// Defer the destroy until after the function completes
	defer terraform.Destroy(t, terraformOptions)

	// Defer the workspace selection until after the function completes
	// to ensure the right state file is used for destroy operations
	defer terraform.WorkspaceSelectOrNew(t, terraformOptions, workspace)

	// Init and apply the terraform module
	ApplyOutput = terraform.InitAndApplyAndIdempotent(t, terraformOptions)

	// Get the number of resources that are created
	resourceCount = terraform.GetResourceCount(t, ApplyOutput)

	// Verify all resources were created
	assert.Equal(t, 11, resourceCount.Add)
	assert.Equal(t, 0, resourceCount.Change)
	assert.Equal(t, 0, resourceCount.Destroy)

	s3BucketId := terraform.Output(t, terraformOptions, "bucket_id")
	name = tfVars["bucket_name"].(string)
	if len(name) > 63 {
		name = name[:63]
	}

	bucketRegion := "us-west-2"
	assumeS3Client := getAssumeRoleClient(t, *roleName, *cfg, bucketRegion)

	bucketPolicy, err := assumeS3Client.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{
		Bucket: &s3BucketId,
	})

	if err != nil {
		t.Fatalf("error finding bucket policy: %v", err)
	}

	assert.True(t, strings.Contains(*bucketPolicy.Policy, fmt.Sprintf("arn:aws:iam::471112763355:role/%v", argoRole)), true)
	assert.Equal(t, strings.TrimSuffix(name, "-"), s3BucketId)

	testGetObjectVersion(t, s3BucketId, assumeS3Client)
	testDeleteObjectVersion(t, s3BucketId, assumeS3Client)
}

// Test retrieving a specific version of an object for new policy actions
func testGetObjectVersion(t *testing.T, bucketName string, assumeS3Client *s3.Client) {

	objectKey := "test-object"
	objectContent := "This is to test the new policy action."

	role, err := helpers.CreateIamRole(*cfg, awsAccountID)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("successfully created test iam role for GetOjbectVersion: %s", *role)
	defer helpers.DeleteIamRole(t, *cfg, *role)

	putObjectOutput, err := assumeS3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &bucketName,
		Key:    &objectKey,
		Body:   strings.NewReader(objectContent),
	})
	if err != nil {
		t.Fatalf("error uploading object: %v", err)
	} else {
		t.Logf("successfully uploaded object: %s", objectKey)
	}

	versionId := putObjectOutput.VersionId

	getObjectOutput, err := assumeS3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket:    &bucketName,
		Key:       &objectKey,
		VersionId: versionId,
	})
	if err != nil {
		t.Fatalf("error getting object version: %v", err)
	}

	defer getObjectOutput.Body.Close()
	buf := new(strings.Builder)
	_, err = io.Copy(buf, getObjectOutput.Body)
	if err != nil {
		t.Fatalf("error reading object content: %v", err)
	}

	assert.Equal(t, objectContent, buf.String())
}

// Test deleting a specific version of an object
func testDeleteObjectVersion(t *testing.T, bucketName string, assumeS3Client *s3.Client) {

	role, err := helpers.CreateIamRole(*cfg, awsAccountID)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("successfully created test iam role for DeleteObjectVerion: %s", *role)
	defer helpers.DeleteIamRole(t, *cfg, *role)

	objectKey := "test-object"

	putObjectOutput, err := assumeS3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &bucketName,
		Key:    &objectKey,
		Body:   strings.NewReader("This is a test object content."),
	})
	if err != nil {
		t.Fatalf("error uploading object: %v", err)
	}

	versionId := putObjectOutput.VersionId

	_, err = assumeS3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket:    &bucketName,
		Key:       &objectKey,
		VersionId: versionId,
	})
	if err != nil {
		t.Fatalf("error deleting object version: %v", err)
	}

	_, err = assumeS3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket:    &bucketName,
		Key:       &objectKey,
		VersionId: versionId,
	})
	assert.Error(t, err)
}
