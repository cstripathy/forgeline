// Package awsdeploy embeds the CloudFormation templates the console can deploy.
package awsdeploy

import _ "embed"

// DemoTemplate is deploy/aws/forgeline-demo.yaml: one private EC2 instance running Forgeline.
//
//go:embed forgeline-demo.yaml
var DemoTemplate string

// DevBoxTemplate is deploy/aws/dev-box.yaml: a private, encrypted EC2 box for one person.
//
//go:embed dev-box.yaml
var DevBoxTemplate string

// QASandboxTemplate is deploy/aws/qa-sandbox.yaml: a DynamoDB table and an SQS queue for a test run.
//
//go:embed qa-sandbox.yaml
var QASandboxTemplate string

// LambdaSandboxTemplate is deploy/aws/lambda-sandbox.yaml: one small function behind a private URL.
//
//go:embed lambda-sandbox.yaml
var LambdaSandboxTemplate string

// EventPipelineTemplate is deploy/aws/event-pipeline.yaml: an SNS topic, an SQS queue and a dead-letter queue.
//
//go:embed event-pipeline.yaml
var EventPipelineTemplate string

// WorkflowSandboxTemplate is deploy/aws/workflow-sandbox.yaml: a Step Functions state machine that calls nothing.
//
//go:embed workflow-sandbox.yaml
var WorkflowSandboxTemplate string
