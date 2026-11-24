#!/bin/sh
set -eu
stage="${1:?usage: app.sh preview|production}"
case "$stage" in preview|production) ;; *) echo "invalid stage" >&2; exit 2;; esac
: "${AWS_REGION:?set AWS_REGION}"
: "${IMAGE_URI:?set IMAGE_URI to ECR digest URI}"
: "${OPERATOR_CIDR:?set OPERATOR_CIDR}"
: "${HOSTED_ZONE_ID:?set HOSTED_ZONE_ID}"
: "${HOSTNAME:?set HOSTNAME}"
: "${CERTIFICATE_ARN:?set CERTIFICATE_ARN}"
if [ "$OPERATOR_CIDR" = "0.0.0.0/0" ]; then echo "refusing public fault-control access" >&2; exit 2; fi
case "$IMAGE_URI" in *@sha256:????????????????????????????????????????????????????????????????) ;; *) echo "IMAGE_URI must be pinned to sha256 digest" >&2; exit 2;; esac
case "$IMAGE_URI" in */dns-failure-lab-"$stage"@sha256:*) ;; *) echo "IMAGE_URI must use the selected stage repository" >&2; exit 2;; esac
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
stack="dns-lab-${stage}"
aws cloudformation deploy --region "$AWS_REGION" --stack-name "$stack" --template-file "$root/infra/app.yaml" --capabilities CAPABILITY_IAM --parameter-overrides "Stage=$stage" "ImageUri=$IMAGE_URI" "OperatorCidr=$OPERATOR_CIDR" "HostedZoneId=$HOSTED_ZONE_ID" "Hostname=$HOSTNAME" "CertificateArn=$CERTIFICATE_ARN" --no-fail-on-empty-changeset 1>&2
cluster=$(aws cloudformation describe-stacks --region "$AWS_REGION" --stack-name "$stack" --query 'Stacks[0].Outputs[?OutputKey==`ClusterName`].OutputValue | [0]' --output text)
service=$(aws cloudformation describe-stacks --region "$AWS_REGION" --stack-name "$stack" --query 'Stacks[0].Outputs[?OutputKey==`ServiceName`].OutputValue | [0]' --output text)
aws ecs wait services-stable --region "$AWS_REGION" --cluster "$cluster" --services "$service"
aws cloudformation describe-stacks --region "$AWS_REGION" --stack-name "$stack" --query 'Stacks[0].Outputs[?OutputKey==`URL`].OutputValue | [0]' --output text
