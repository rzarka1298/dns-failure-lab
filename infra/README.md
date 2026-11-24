# AWS preview and production deployment

This directory contains a reviewable AWS deployment path. **No AWS resources are created by adding these files.** The release workflow is manual (`workflow_dispatch`). It deploys preview, runs a synthetic smoke probe and bounded labeled benchmark, then optionally promotes the **same OCI image** to production after GitHub's production environment approval. Preview and production each have a separate VPC, ECS cluster/service, load balancer, log group, and immutable ECR repository.

## Access boundary and cost

The workbench can change DNS faults through unauthenticated HTTP endpoints. Each load balancer accepts **HTTPS only from one configured operator/runner CIDR**; the task accepts port 8080 only from that load balancer. Do not set the CIDR to `0.0.0.0/0`. Set the CIDR to a narrow static egress range, ideally `/32`, and put the deploy runner and operator behind that range. The app has no user authentication, so network access remains the security boundary. The smoke test changes the stage to `healthy` and sends one synthetic probe.

AWS will charge for the application load balancers, Fargate tasks, CloudWatch logs, ECR storage, Route 53, and data transfer. Set an AWS Budget and review current pricing before deploying. This template uses one task per environment and does not promise high availability. It does not create a NAT gateway.

## Prerequisites

1. An AWS account, AWS CLI v2, Docker, Python 3, curl, a Route 53 public hosted zone, and ACM certificates in the deployment region for distinct preview and production hostnames.
2. A GitHub repository with OIDC trust set up for AWS. Create **separate** preview and production deploy roles. Trust only your repository's `environment:preview` or `environment:production` OIDC subject, respectively, and `aud=sts.amazonaws.com`. Give each role the CloudFormation, ECS, ECR, EC2 networking, ELB, Route 53, CloudWatch Logs, IAM `PassRole`, and role creation permissions needed by these templates, scoped to this lab where feasible. The production role also needs ECR pull access to the preview repository to promote the same image. Do not use long-lived AWS keys.
3. A Linux self-hosted GitHub Actions runner labeled `dns-lab-deploy`, with AWS CLI, Docker, Python 3, and curl. Its public egress IP must be inside both allowed CIDRs. Protect the runner from untrusted workflows and repository forks.
4. GitHub environments named `preview` and `production`. Set **required reviewers** on `production`; restrict environment deployment branches to `main`. Give the OIDC roles matching trust conditions. Restrict manual release and rollback workflow access to trusted maintainers.
5. Repository variables: `AWS_REGION`, `AWS_PREVIEW_ROLE_ARN`, `AWS_PRODUCTION_ROLE_ARN`, `DNS_HOSTED_ZONE_ID`, `PREVIEW_HOSTNAME`, `PRODUCTION_HOSTNAME`, `PREVIEW_CERTIFICATE_ARN`, `PRODUCTION_CERTIFICATE_ARN`, `PREVIEW_OPERATOR_CIDR`, `PRODUCTION_OPERATOR_CIDR`. Hostnames must be covered by the corresponding ACM certificate. Use different hostnames and narrow CIDRs.

The role trust condition for preview should have this shape (replace account, provider, owner, and repository):

```json
{
  "StringEquals": {
    "token.actions.githubusercontent.com:aud": "sts.amazonaws.com",
    "token.actions.githubusercontent.com:sub": "repo:OWNER/REPO:environment:preview"
  }
}
```

Use `environment:production` for the production role. GitHub recommends environment protection rules with OIDC; the environment name becomes part of the subject claim.

## Release

Run **Actions → release → Run workflow** on `main`. With `promote_production=false`, the workflow tests, builds, pushes an immutable preview image, deploys preview, runs `deploy/smoke.sh`, and saves benchmark evidence. The preview benchmark must report perfect precision and recall for its deterministic scenario sequence and at least one detection and recovery measurement. With `promote_production=true`, it waits for the protected production environment, copies the exact preview image into production ECR, deploys production, and smoke tests it. The deploy scripts pin images to `sha256` digests. Each ECS service uses a deployment circuit breaker with rollback for deployments that fail before reaching a steady state.

The equivalent operator commands, after assuming a stage-specific role and setting `AWS_REGION`, are:

```sh
repo=$(sh deploy/registry.sh preview)
# Build and push a tagged image to "$repo" using docker; obtain its ECR sha256 digest.
export IMAGE_URI="$repo@sha256:YOUR_64_CHARACTER_DIGEST"
export OPERATOR_CIDR="YOUR_STATIC_EGRESS_IP/32"
export HOSTED_ZONE_ID="YOUR_ROUTE53_ZONE_ID"
export HOSTNAME="preview.example.com"
export CERTIFICATE_ARN="YOUR_ACM_CERTIFICATE_ARN"
url=$(sh deploy/app.sh preview)
sh deploy/smoke.sh "$url"
```

`deploy/app.sh` waits for ECS service stability and prints the stage URL. It cannot prove the application is healthy without the smoke test. A runner outside the configured CIDR cannot access the workbench, as intended.

## Rollback

ECS automatically rolls back a deployment that fails its circuit breaker if a prior deployment completed successfully. For a release that reached steady state but later proves bad, run **Actions → rollback → Run workflow**, select the stage and enter the 64-character digest of a known-good image in that stage's ECR repository. The production environment reviewer gate applies. The workflow verifies that digest exists, deploys it, waits for stability, and runs the synthetic smoke probe.

From a machine with stage credentials and the same environment variables as above:

```sh
sh deploy/rollback.sh production YOUR_KNOWN_GOOD_64_CHARACTER_DIGEST
```

Record the successful image digest after each release. ECR retains the last 30 images; older images may expire. Review CloudFormation events and ECS service events if deployment or rollback fails.

## Validation and limits

The `test` GitHub Actions workflow starts the local Compose lab, runs a live smoke probe and bounded synthetic benchmark, checks detection and recovery evidence, and uploads the report and container logs. The manual release workflow calls those checks before building preview.

For offline CloudFormation validation without AWS credentials, install AWS's linter and run it on both templates:

```sh
python3 -m pip install cfn-lint
cfn-lint infra/registry.yaml infra/app.yaml
```

`cfn-lint` checks resource schemas and references locally, but cannot prove an AWS account has the required permissions or quotas. A real AWS CloudFormation `validate-template`, deployment, network smoke/benchmark test, and rollback rehearsal require configured AWS access and were not run in this sandbox. The current service keeps events in memory; a task replacement resets the timeline. Logs go to CloudWatch, but durable metrics/traces export remains future work.

References: [GitHub OIDC for AWS](https://docs.github.com/en/actions/how-tos/secure-your-work/security-harden-deployments/oidc-in-aws), [ECS deployment failure detection](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/deployment-failure-detection.html), [CloudFormation ECS service](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-resource-ecs-service.html), [ECS task load balancers](https://docs.aws.amazon.com/AWSCloudFormation/latest/TemplateReference/aws-properties-ecs-service-loadbalancer.html), [AWS cfn-lint guidance](https://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/cfn-lint.html).
