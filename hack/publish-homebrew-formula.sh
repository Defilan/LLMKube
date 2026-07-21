#!/usr/bin/env bash
# Render Formula/llmkube.rb, Formula/llmkube-foreman-agent.rb, and
# Formula/llmkube-metal-agent.rb from the release checksums and publish them
# to the Homebrew tap.
#
# Why a hand-maintained formula: GoReleaser removed the `brews` option in v2.16
# (formulae are meant to build from source, so it steers binary distribution to
# `homebrew_casks`). But casks are macOS-only, which drops Linux Homebrew for a
# cross-platform CLI. A binary-download formula selects the right prebuilt
# archive per OS/arch and works on macOS AND Linux from one artifact and one
# `brew install defilantech/tap/llmkube`. See defilantech/LLMKube#1039 / #1040.
#
# Usage: publish-homebrew-formula.sh <version> [checksums-file]
#   DRY_RUN=1 prints the rendered formulae and exits (no clone, no push).
set -euo pipefail

VERSION="${1:?usage: publish-homebrew-formula.sh <version> [checksums-file]}"
CHECKSUMS="${2:-dist/checksums.txt}"
TAP_SLUG="defilantech/homebrew-tap"

# sha_for prints the sha256 of the given archive for the given os_arch, reading
# GoReleaser's "<sha256>  <filename>" checksums file.
#   sha_for <archive-prefix> <os_arch>
#   e.g. sha_for LLMKube darwin_arm64
#        sha_for LLMKube-foreman-agent darwin_arm64
sha_for() {
  local prefix="$1" arch="$2" line
  line=$(grep -E "[[:space:]]${prefix}_${VERSION}_${arch}\.tar\.gz\$" "$CHECKSUMS" || true)
  [ -n "$line" ] || { echo "no checksum for ${prefix}_${VERSION}_${arch}.tar.gz in $CHECKSUMS" >&2; exit 1; }
  echo "${line%% *}"
}

# CLI (llmkube) — cross-platform
DARWIN_ARM_CLI=$(sha_for LLMKube darwin_arm64)
DARWIN_AMD_CLI=$(sha_for LLMKube darwin_amd64)
LINUX_ARM_CLI=$(sha_for LLMKube linux_arm64)
LINUX_AMD_CLI=$(sha_for LLMKube linux_amd64)

# foreman-agent — darwin (edge nodes) and linux (in-cluster)
DARWIN_ARM_FA=$(sha_for LLMKube-foreman-agent darwin_arm64)
DARWIN_AMD_FA=$(sha_for LLMKube-foreman-agent darwin_amd64)
LINUX_ARM_FA=$(sha_for LLMKube-foreman-agent linux_arm64)
LINUX_AMD_FA=$(sha_for LLMKube-foreman-agent linux_amd64)

# metal-agent — darwin only (Metal is macOS-specific)
DARWIN_ARM_MA=$(sha_for LLMKube-metal-agent darwin_arm64)
DARWIN_AMD_MA=$(sha_for LLMKube-metal-agent darwin_amd64)

render_llmkube() {
  cat <<EOF
class Llmkube < Formula
  desc "GPU-accelerated Kubernetes operator for local LLM inference"
  homepage "https://github.com/defilantech/LLMKube"
  version "${VERSION}"
  license "Apache-2.0"

  on_macos do
    on_arm do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube_${VERSION}_darwin_arm64.tar.gz"
      sha256 "${DARWIN_ARM_CLI}"
    end
    on_intel do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube_${VERSION}_darwin_amd64.tar.gz"
      sha256 "${DARWIN_AMD_CLI}"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube_${VERSION}_linux_arm64.tar.gz"
      sha256 "${LINUX_ARM_CLI}"
    end
    on_intel do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube_${VERSION}_linux_amd64.tar.gz"
      sha256 "${LINUX_AMD_CLI}"
    end
  end

  def install
    bin.install "llmkube"
  end

  test do
    assert_match "llmkube", shell_output("#{bin}/llmkube version 2>&1")
  end
end
EOF
}

render_foreman_agent() {
  cat <<EOF
class LlmkubeForemanAgent < Formula
  desc "Foreman agent for off-cluster LLMKube fleet nodes"
  homepage "https://github.com/defilantech/LLMKube"
  version "${VERSION}"
  license "Apache-2.0"

  on_macos do
    on_arm do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube-foreman-agent_${VERSION}_darwin_arm64.tar.gz"
      sha256 "${DARWIN_ARM_FA}"
    end
    on_intel do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube-foreman-agent_${VERSION}_darwin_amd64.tar.gz"
      sha256 "${DARWIN_AMD_FA}"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube-foreman-agent_${VERSION}_linux_arm64.tar.gz"
      sha256 "${LINUX_ARM_FA}"
    end
    on_intel do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube-foreman-agent_${VERSION}_linux_amd64.tar.gz"
      sha256 "${LINUX_AMD_FA}"
    end
  end

  def install
    bin.install "foreman-agent"
  end

  test do
    assert_match "foreman-agent", shell_output("#{bin}/foreman-agent --version 2>&1")
  end
end
EOF
}

render_metal_agent() {
  cat <<EOF
class LlmkubeMetalAgent < Formula
  desc "Metal agent for off-cluster LLMKube inference on Apple Silicon"
  homepage "https://github.com/defilantech/LLMKube"
  version "${VERSION}"
  license "Apache-2.0"

  on_macos do
    on_arm do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube-metal-agent_${VERSION}_darwin_arm64.tar.gz"
      sha256 "${DARWIN_ARM_MA}"
    end
    on_intel do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube-metal-agent_${VERSION}_darwin_amd64.tar.gz"
      sha256 "${DARWIN_AMD_MA}"
    end
  end

  def install
    bin.install "llmkube-metal-agent"
  end

  test do
    assert_match "llmkube-metal-agent", shell_output("#{bin}/llmkube-metal-agent --version 2>&1")
  end
end
EOF
}

if [ "${DRY_RUN:-}" = "1" ]; then
  render_llmkube
  echo "---"
  render_foreman_agent
  echo "---"
  render_metal_agent
  exit 0
fi

: "${HOMEBREW_TAP_TOKEN:?HOMEBREW_TAP_TOKEN is required to push the tap}"
workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT
git clone --depth 1 "https://x-access-token:${HOMEBREW_TAP_TOKEN}@github.com/${TAP_SLUG}.git" "$workdir/tap"

mkdir -p "$workdir/tap/Formula"
render_llmkube >"$workdir/tap/Formula/llmkube.rb"
render_foreman_agent >"$workdir/tap/Formula/llmkube-foreman-agent.rb"
render_metal_agent >"$workdir/tap/Formula/llmkube-metal-agent.rb"
# Retire the macOS-only cask; the cross-platform formula supersedes it.
git -C "$workdir/tap" rm -f --ignore-unmatch Casks/llmkube.rb
git -C "$workdir/tap" add Formula/llmkube.rb Formula/llmkube-foreman-agent.rb Formula/llmkube-metal-agent.rb

if git -C "$workdir/tap" diff --cached --quiet; then
  echo "formulae already at v${VERSION}; nothing to publish"
  exit 0
fi

git -C "$workdir/tap" \
  -c user.name="github-actions[bot]" \
  -c user.email="41898282+github-actions[bot]@users.noreply.github.com" \
  commit -m "Update llmkube formulae to v${VERSION}"
git -C "$workdir/tap" push
echo "published llmkube, llmkube-foreman-agent, llmkube-metal-agent formulae v${VERSION} to ${TAP_SLUG}"
