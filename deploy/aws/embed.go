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
