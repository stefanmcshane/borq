#!/usr/bin/env bash
set -euo pipefail

IMAGE_REPO="${IMAGE_REPO:-ghcr.io/stefanmcshane/borq}"
RC_IMAGE_REF="${RC_IMAGE_REF:-${BORQ_INTEGRATION_RC_IMAGE:-}}"
RELEASE_TAG="${RELEASE_TAG:-${RELEASE_STAMP:-}}"

if [[ -z "${RC_IMAGE_REF}" ]] && [[ -f .release/last-rc-image.env ]]; then
  # shellcheck disable=SC1091
  source .release/last-rc-image.env
  RC_IMAGE_REF="${RC_DIGEST_REF:-${RC_IMAGE:-}}"
  IMAGE_REPO="${IMAGE_REPO:-${IMAGE_REPO}}"
  RELEASE_TAG="${RELEASE_TAG:-${RELEASE_STAMP:-}}"
fi

if [[ -z "${RC_IMAGE_REF}" ]]; then
  echo "set RC_IMAGE_REF (or BORQ_INTEGRATION_RC_IMAGE) to the tested RC image tag or digest" >&2
  exit 1
fi

if [[ -z "${RELEASE_TAG}" ]]; then
  RELEASE_TAG="$(date -u +%Y%m%d-%H%M)"
fi

if [[ "${RC_IMAGE_REF}" == *"@sha256:"* ]]; then
  DIGEST_REF="${RC_IMAGE_REF}"
else
  digest="$(docker buildx imagetools inspect "${RC_IMAGE_REF}" | awk '/Digest:/ {print $2; exit}')"
  if [[ -z "${digest}" ]]; then
    echo "unable to resolve digest for ${RC_IMAGE_REF}" >&2
    exit 1
  fi
  DIGEST_REF="${IMAGE_REPO}@${digest}"
fi

docker buildx imagetools create \
  -t "${IMAGE_REPO}:${RELEASE_TAG}" \
  -t "${IMAGE_REPO}:latest" \
  "${DIGEST_REF}"

echo "promoted digest ${DIGEST_REF} to ${IMAGE_REPO}:${RELEASE_TAG} and ${IMAGE_REPO}:latest"
