# shellcheck shell=bash disable=SC2034 # the scripts that source this use the settings
# Shared settings for the hack/ scripts. Source this file; don't run it.
#
# Every tool version is pinned here, so `make demo` behaves the same on every
# machine. The scripts download their own kind, kubectl and helm into ./bin and
# never touch your ~/.kube/config: the demo cluster gets its own kubeconfig in
# ./.demo.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DIR="$ROOT_DIR/bin"
DEMO_DIR="$ROOT_DIR/.demo"

KIND_VERSION="v0.33.0"
KUBECTL_VERSION="v1.37.3"
HELM_VERSION="v4.3.1"
# Default node image of kind v0.33.0, pinned by digest.
KIND_NODE_IMAGE="kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5"
# Kubernetes 1.35 and later refuse to run on cgroup v1, which older Docker
# Desktop (WSL2) setups still use. On those hosts the demo uses this image,
# also from the kind v0.33.0 release notes.
KIND_NODE_IMAGE_CGROUP_V1="kindest/node:v1.34.11@sha256:44e222ee2132dab25ff87301682f89eb82c7880ea3a1bf543bfe9708fd08d67d"

FLAGSMITH_CHART_REPO="https://flagsmith.github.io/flagsmith-charts/"
FLAGSMITH_CHART_VERSION="0.85.0"
# Flagsmith release the demo runs. Images come from Docker Hub.
FLAGSMITH_VERSION="2.295.2"

CLUSTER_NAME="${CLUSTER_NAME:-flagbreaker-demo}"
FLAGSMITH_NAMESPACE="flagsmith"
FLAGSMITH_RELEASE="flagsmith"

# Ports on your machine. kind maps them to NodePorts inside the cluster.
API_PORT="${API_PORT:-8000}"
UI_PORT="${UI_PORT:-8080}"
API_URL="http://localhost:${API_PORT}"
UI_URL="http://localhost:${UI_PORT}"

case "$(uname -s)" in
  Linux*) OS=linux ;;
  Darwin*) OS=darwin ;;
  MINGW* | MSYS* | CYGWIN*) OS=windows ;;
  *) echo "unsupported OS: $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  arm64 | aarch64) ARCH=arm64 ;;
  *) echo "unsupported CPU: $(uname -m)" >&2; exit 1 ;;
esac
EXE=""
[[ "$OS" == windows ]] && EXE=".exe"

# Native Windows tools (kubectl.exe, helm.exe) need C:/... paths, not /c/...
native_path() {
  if [[ "$OS" == windows ]] && command -v cygpath >/dev/null 2>&1; then
    cygpath -m "$1"
  else
    printf '%s' "$1"
  fi
}

KIND="$BIN_DIR/kind$EXE"
KUBECTL="$BIN_DIR/kubectl$EXE"
HELM="$BIN_DIR/helm$EXE"

mkdir -p "$DEMO_DIR"
export KUBECONFIG
KUBECONFIG="$(native_path "$DEMO_DIR/kubeconfig")"

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

require() {
  local missing=()
  for cmd in "$@"; do
    command -v "$cmd" >/dev/null 2>&1 || missing+=("$cmd")
  done
  ((${#missing[@]} == 0)) || die "please install: ${missing[*]}"
}
