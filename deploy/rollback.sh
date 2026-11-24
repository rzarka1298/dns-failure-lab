#!/bin/sh
set -eu
stage="${1:?usage: rollback.sh preview|production 64-char-digest}"
digest="${2:?usage: rollback.sh preview|production 64-char-digest}"
case "$stage" in preview|production) ;; *) echo "invalid stage" >&2; exit 2;; esac
case "$digest" in *[!a-f0-9]*|'') echo "digest must be lowercase hex" >&2; exit 2;; esac
if [ "${#digest}" -ne 64 ]; then echo "digest must be 64 characters" >&2; exit 2; fi
: "${AWS_REGION:?set AWS_REGION}"
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
repository="dns-failure-lab-${stage}"
aws ecr describe-images --region "$AWS_REGION" --repository-name "$repository" --image-ids "imageDigest=sha256:$digest" --query 'imageDetails[0].imageDigest' --output text >/dev/null
repository_uri=$(aws cloudformation describe-stacks --region "$AWS_REGION" --stack-name "dns-lab-${stage}-registry" --query 'Stacks[0].Outputs[?OutputKey==`RepositoryUri`].OutputValue | [0]' --output text)
IMAGE_URI="${repository_uri}@sha256:${digest}"
export IMAGE_URI
url=$("$root/deploy/app.sh" "$stage")
"$root/deploy/smoke.sh" "$url"
