// Package awsdeploy embeds the CloudFormation templates the console can deploy.
package awsdeploy

import _ "embed"

// DemoTemplate is deploy/aws/forgeline-demo.yaml: one private EC2 instance running Forgeline.
//
//go:embed forgeline-demo.yaml
var DemoTemplate string
