#!/bin/sh
set -eu
stage="${1:?usage: registry.sh preview|production}"
case "$stage" in preview|production) ;; *) echo "invalid stage" >&2; exit 2;; esac
: "${AWS_REGION:?set AWS_REGION}"
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
stack="dns-lab-${stage}-registry"
aws cloudformation deploy --region "$AWS_REGION" --stack-name "$stack" --template-file "$root/infra/registry.yaml" --parameter-overrides "Stage=$stage" --no-fail-on-empty-changeset 1>&2
aws cloudformation describe-stacks --region "$AWS_REGION" --stack-name "$stack" --query 'Stacks[0].Outputs[?OutputKey==`RepositoryUri`].OutputValue | [0]' --output text
