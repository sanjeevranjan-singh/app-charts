#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BUILD_CONTEXT="$(cd "$SCRIPT_DIR/.." && pwd)"

IMAGE_REGISTRY="${IMAGE_REGISTRY:-agentcert}"
IMAGE_NAME="${IMAGE_NAME:-agentcert-install-app}"
IMAGE_TAG="${IMAGE_TAG:-ci-$(date +%Y%m%d%H%M%S)}"
MINIKUBE_PROFILE="${MINIKUBE_PROFILE:-minikube}"
TAG_LATEST="${TAG_LATEST:-true}"
NO_CACHE="${NO_CACHE:-false}"
AGENTCERT_ENV_FILE="${AGENTCERT_ENV_FILE:-$BUILD_CONTEXT/../AgentCert/local-custom/config/.env}"
LOCAL_MODE=false

SOCKSHOP_VALUES_FILE="$BUILD_CONTEXT/charts/sock-shop/values.yaml"
SOCKSHOP_VALUES_BACKUP=""

IMAGE_REPO="${IMAGE_REGISTRY}/${IMAGE_NAME}"
PRIMARY_IMAGE="${IMAGE_REPO}:${IMAGE_TAG}"
LATEST_IMAGE="${IMAGE_REPO}:latest"

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Build and load install-app image into minikube, then update AgentCert .env.

Options:
  --local-mode   Build with local-friendly sock-shop tracing settings
                 (temporary override: tracing.disableSleuth=true, tracing.zipkinHost="")
  --help         Show this help
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --local-mode)
            LOCAL_MODE=true
            shift
            ;;
        --help|-h)
            usage
            exit 0
            ;;
        *)
            printf '[ERROR] Unknown option: %s\n' "$1" >&2
            usage
            exit 1
            ;;
    esac
done

info() {
    printf '\n[INFO] %s\n' "$1"
}

success() {
    printf '[OK] %s\n' "$1"
}

warn() {
    printf '[WARN] %s\n' "$1"
}

require_cmd() {
    command -v "$1" >/dev/null 2>&1 || {
        printf '[ERROR] Required command not found: %s\n' "$1" >&2
        exit 1
    }
}

restore_sockshop_values() {
    if [[ -n "$SOCKSHOP_VALUES_BACKUP" && -f "$SOCKSHOP_VALUES_BACKUP" ]]; then
        mv "$SOCKSHOP_VALUES_BACKUP" "$SOCKSHOP_VALUES_FILE"
        success "Restored sock-shop values.yaml defaults"
    fi
}

prepare_local_mode_overrides() {
    if [[ "$LOCAL_MODE" != "true" ]]; then
        return 0
    fi

    if [[ ! -f "$SOCKSHOP_VALUES_FILE" ]]; then
        warn "sock-shop values not found at $SOCKSHOP_VALUES_FILE; skipping local-mode chart overrides"
        return 0
    fi

    info "Applying local-mode tracing overrides to sock-shop chart (temporary)"
    SOCKSHOP_VALUES_BACKUP="${SOCKSHOP_VALUES_FILE}.bak.$(date +%Y%m%d%H%M%S)"
    cp "$SOCKSHOP_VALUES_FILE" "$SOCKSHOP_VALUES_BACKUP"

    sed -i 's|^\([[:space:]]*zipkinHost:[[:space:]]*\).*|\1""|' "$SOCKSHOP_VALUES_FILE"
    sed -i 's|^\([[:space:]]*disableSleuth:[[:space:]]*\).*|\1true|' "$SOCKSHOP_VALUES_FILE"

    trap restore_sockshop_values EXIT
    success "Local-mode overrides applied: tracing.disableSleuth=true, tracing.zipkinHost=\"\""
}

upsert_env_value() {
    local key="$1"
    local value="$2"
    local escaped
    escaped=$(printf '%s' "$value" | sed 's/[&/\\]/\\&/g')

    if grep -q -E "^${key}=" "$AGENTCERT_ENV_FILE"; then
        sed -i "s/^${key}=.*/${key}=${escaped}/" "$AGENTCERT_ENV_FILE"
    else
        printf '\n%s=%s\n' "$key" "$value" >> "$AGENTCERT_ENV_FILE"
    fi
}

build_image() {
    local build_args=(docker build -t "$PRIMARY_IMAGE" -f "$SCRIPT_DIR/Dockerfile")
    if [[ "$NO_CACHE" == "true" ]]; then
        build_args+=(--no-cache)
    fi
    build_args+=("$BUILD_CONTEXT")

    info "Building ${PRIMARY_IMAGE}"
    "${build_args[@]}"

    if [[ "$TAG_LATEST" == "true" ]]; then
        info "Tagging ${PRIMARY_IMAGE} as ${LATEST_IMAGE}"
        docker tag "$PRIMARY_IMAGE" "$LATEST_IMAGE"
    fi

    success "Docker build completed"
}

load_into_minikube() {
    info "Loading ${PRIMARY_IMAGE} into ${MINIKUBE_PROFILE}"
    minikube -p "$MINIKUBE_PROFILE" image load "$PRIMARY_IMAGE"

    if [[ "$TAG_LATEST" == "true" ]]; then
        info "Loading ${LATEST_IMAGE} into ${MINIKUBE_PROFILE}"
        minikube -p "$MINIKUBE_PROFILE" image load "$LATEST_IMAGE"
    fi

    success "Images loaded into minikube"
}

prune_local_images() {
    local keep_refs=("$PRIMARY_IMAGE")
    if [[ "$TAG_LATEST" == "true" ]]; then
        keep_refs+=("$LATEST_IMAGE")
    fi

    info "Pruning older local Docker images for ${IMAGE_REPO}"
    while IFS= read -r ref; do
        [[ -z "$ref" || "$ref" == "<none>:<none>" ]] && continue

        local keep=false
        for wanted in "${keep_refs[@]}"; do
            if [[ "$ref" == "$wanted" ]]; then
                keep=true
                break
            fi
        done

        if [[ "$keep" == "false" ]]; then
            docker rmi -f "$ref" >/dev/null 2>&1 || warn "Failed to remove local image ${ref}"
        fi
    done < <(docker images "$IMAGE_REPO" --format '{{.Repository}}:{{.Tag}}' | sort -u)

    docker image prune -f >/dev/null 2>&1 || true
    success "Local Docker image prune complete"
}

prune_minikube_images() {
    local keep_refs=("$PRIMARY_IMAGE")
    if [[ "$TAG_LATEST" == "true" ]]; then
        keep_refs+=("$LATEST_IMAGE")
    fi

    info "Pruning older ${IMAGE_REPO} images from ${MINIKUBE_PROFILE}"
    while IFS= read -r ref; do
        [[ -z "$ref" ]] && continue

        local keep=false
        for wanted in "${keep_refs[@]}"; do
            # minikube image ls prefixes with docker.io/ — strip it for comparison
            local ref_bare="${ref#docker.io/}"
            local wanted_bare="${wanted#docker.io/}"
            if [[ "$ref_bare" == "$wanted_bare" ]]; then
                keep=true
                break
            fi
        done

        if [[ "$keep" == "false" ]]; then
            minikube -p "$MINIKUBE_PROFILE" image rm "$ref" >/dev/null 2>&1 || warn "Failed to remove minikube image ${ref}"
        fi
    done < <(minikube -p "$MINIKUBE_PROFILE" image ls 2>/dev/null | grep -E "(^|/)${IMAGE_REPO##*/}:" | sort -u || true)

    success "Minikube image prune complete"
}

update_agentcert_env() {
    if [[ ! -f "$AGENTCERT_ENV_FILE" ]]; then
        warn "AgentCert .env not found at ${AGENTCERT_ENV_FILE}; skipping env update"
        return 0
    fi

    info "Updating AgentCert .env with ${PRIMARY_IMAGE}"
    upsert_env_value "INSTALL_APPLICATION_IMAGE" "$PRIMARY_IMAGE"
    success "AgentCert .env updated: INSTALL_APPLICATION_IMAGE=${PRIMARY_IMAGE}"
}

show_result() {
    printf '\nBuilt image: %s\n' "$PRIMARY_IMAGE"
    if [[ "$TAG_LATEST" == "true" ]]; then
        printf 'Alias image: %s\n' "$LATEST_IMAGE"
    fi
    printf 'Minikube profile: %s\n' "$MINIKUBE_PROFILE"
    printf 'Updated .env: %s\n' "$AGENTCERT_ENV_FILE"
}

main() {
    require_cmd docker
    require_cmd minikube

    prepare_local_mode_overrides

    build_image
    load_into_minikube
    prune_local_images
    prune_minikube_images
    update_agentcert_env
    show_result
}

main "$@"