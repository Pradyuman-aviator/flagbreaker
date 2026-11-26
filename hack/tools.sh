#!/usr/bin/env bash
# Downloads the pinned kind, kubectl and helm into ./bin and checks each
# download against the SHA-256 pinned in hack/lib.sh (copied from each
# project's release). Skips tools that are already there.

source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
require curl tar

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# fetch URL DEST EXPECTED_SHA256
fetch() {
  local url=$1 dest=$2 want=$3 got
  curl -fsSL --retry 3 -o "$dest" "$url"
  got="$(sha256 "$dest")"
  if [[ "$got" != "$want" ]]; then
    rm -f "$dest"
    die "checksum mismatch for $url (want $want, got $got)"
  fi
}

mkdir -p "$BIN_DIR"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

if [[ ! -x "$KIND" ]]; then
  log "downloading kind $KIND_VERSION"
  base="https://github.com/kubernetes-sigs/kind/releases/download/$KIND_VERSION/kind-$OS-$ARCH"
  want="$(pinned_sha256 kind)" || die "kind $KIND_VERSION has no build for $OS/$ARCH"
  fetch "$base" "$KIND" "$want"
  chmod +x "$KIND"
fi

if [[ ! -x "$KUBECTL" ]]; then
  log "downloading kubectl $KUBECTL_VERSION"
  base="https://dl.k8s.io/release/$KUBECTL_VERSION/bin/$OS/$ARCH/kubectl$EXE"
  want="$(pinned_sha256 kubectl)" || die "no pinned kubectl checksum for $OS/$ARCH"
  fetch "$base" "$KUBECTL" "$want"
  chmod +x "$KUBECTL"
fi

if [[ ! -x "$HELM" ]]; then
  log "downloading helm $HELM_VERSION"
  if [[ "$OS" == windows ]]; then
    require unzip
    archive="helm-$HELM_VERSION-$OS-$ARCH.zip"
  else
    archive="helm-$HELM_VERSION-$OS-$ARCH.tar.gz"
  fi
  want="$(pinned_sha256 helm)" || die "no pinned helm checksum for $OS/$ARCH"
  fetch "https://get.helm.sh/$archive" "$tmp/$archive" "$want"
  if [[ "$OS" == windows ]]; then
    unzip -q "$tmp/$archive" -d "$tmp"
  else
    tar -xzf "$tmp/$archive" -C "$tmp"
  fi
  mv "$tmp/$OS-$ARCH/helm$EXE" "$HELM"
  chmod +x "$HELM"
fi

log "tools ready in $BIN_DIR"
"$KIND" version
"$KUBECTL" version --client | head -1
"$HELM" version --short
