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

# pinned_sha256 TOOL -> the SHA-256 published for this tool version, OS and
# CPU, pinned here so a download is checked against a value from this repo,
# not one fetched from the same server as the download.
pinned_sha256() {
  case "$1-$OS-$ARCH" in
    kind-linux-amd64) echo aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d ;;
    kind-linux-arm64) echo 20022bee6cfcd5086cb7234d218e3454e6090022f2a8f55d1fa7fcf42c3867a2 ;;
    kind-darwin-amd64) echo 5a99f26f57246dc9319dd294803313197a0f34d33c525b3ea8b655db5916ece0 ;;
    kind-darwin-arm64) echo 0c8c7dbe5e23594a198b786c4bc13dacc101fa6196b0cb0b23a1ca44e61f4b4f ;;
    kind-windows-amd64) echo 4b22adaa135368c5a465d56bbd8e520cbea87272a06ca00b6078e7b81515c9fc ;;
    kubectl-linux-amd64) echo 9a1b55af2795b25dd288dbef62387db9b5530087ab918d28c6ad067f2e3cb07c ;;
    kubectl-linux-arm64) echo da160b41491cc5c5e575d78bb665810fb900f70fad666d834094b24ec56d304f ;;
    kubectl-darwin-amd64) echo 82db1bc2b466e9f46ed36d609d8c10523a425664e6e648a666df252b9502fe9e ;;
    kubectl-darwin-arm64) echo 7ac1234e7731e902e5ab2153e2ed29bbe7abddd7b0be5ffd94afb7df47da8f27 ;;
    kubectl-windows-amd64) echo 98ee2ce86421b2e2ff24b4d3c081883487694371b8ca2e40e125f413677a5337 ;;
    kubectl-windows-arm64) echo bf55dc93d14e9a49a57224e78088b6bb387564ff94c69090ac0ed22120f7f1f3 ;;
    helm-linux-amd64) echo 0403a88ae20913d11b10a2467b50087721fea7581736da6b9d3992f5357fe54b ;;
    helm-linux-arm64) echo 8cf8f66ecbc252ac8f7f06cca68aba88508ddfde3a6a34feb1678ad1694b4f88 ;;
    helm-darwin-amd64) echo 34bec4eba761ba5c16d1965ee6b8f5b0abe5993974a802274e2a4f0418893d89 ;;
    helm-darwin-arm64) echo 6b4e61c8cd27d99e5a801e63ff25dd5c70b36875c2cd659d3e9ce6b76fc23198 ;;
    helm-windows-amd64) echo fcfd7b796dcf0f225abc40b5636a3b01f55f3de2fb1dfb18475df94e62c14112 ;;
    helm-windows-arm64) echo 373edaae86db70342ef39b014e2e7764ea8f9cff08714843daa5e55b5ee492f2 ;;
    *) return 1 ;;
  esac
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
