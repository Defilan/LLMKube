#!/usr/bin/env bash
# Render Formula/llmkube.rb from the release checksums and publish it to the
# Homebrew tap.
#
# Why a hand-maintained formula: GoReleaser removed the `brews` option in v2.16
# (formulae are meant to build from source, so it steers binary distribution to
# `homebrew_casks`). But casks are macOS-only, which drops Linux Homebrew for a
# cross-platform CLI. A binary-download formula selects the right prebuilt
# archive per OS/arch and works on macOS AND Linux from one artifact and one
# `brew install defilantech/tap/llmkube`. See defilantech/LLMKube#1039 / #1040.
#
# Usage: publish-homebrew-formula.sh <version> [checksums-file]
#   DRY_RUN=1 prints the rendered formula and exits (no clone, no push).
set -euo pipefail

VERSION="${1:?usage: publish-homebrew-formula.sh <version> [checksums-file]}"
CHECKSUMS="${2:-dist/checksums.txt}"
TAP_SLUG="defilantech/homebrew-tap"

# sha_for prints the sha256 of the archive for the given os_arch, reading
# GoReleaser's "<sha256>  <filename>" checksums file.
sha_for() {
  local pattern="$1" line
  line=$(grep -E "[[:space:]]${pattern}\\.tar\\.gz\$" "$CHECKSUMS" || true)
  [ -n "$line" ] || { echo "no checksum for ${pattern}.tar.gz in $CHECKSUMS" >&2; exit 1; }
  echo "${line%% *}"
}

DARWIN_ARM=$(sha_for "LLMKube_${VERSION}_darwin_arm64")
DARWIN_AMD=$(sha_for "LLMKube_${VERSION}_darwin_amd64")
LINUX_ARM=$(sha_for "LLMKube_${VERSION}_linux_arm64")
LINUX_AMD=$(sha_for "LLMKube_${VERSION}_linux_amd64")

# Agent archive checksums. The foreman-agent ships darwin+linux; the metal
# agent is darwin-only (Metal is Apple Silicon / Apple GPU only).
FOREMAN_DARWIN_ARM=$(sha_for "LLMKube-foreman-agent_${VERSION}_darwin_arm64")
FOREMAN_DARWIN_AMD=$(sha_for "LLMKube-foreman-agent_${VERSION}_darwin_amd64")
FOREMAN_LINUX_ARM=$(sha_for "LLMKube-foreman-agent_${VERSION}_linux_arm64")
FOREMAN_LINUX_AMD=$(sha_for "LLMKube-foreman-agent_${VERSION}_linux_amd64")

METAL_DARWIN_ARM=$(sha_for "LLMKube-metal-agent_${VERSION}_darwin_arm64")
METAL_DARWIN_AMD=$(sha_for "LLMKube-metal-agent_${VERSION}_darwin_amd64")

render() {
  cat <<EOF
class Llmkube < Formula
  desc "GPU-accelerated Kubernetes operator for local LLM inference"
  homepage "https://github.com/defilantech/LLMKube"
  version "${VERSION}"
  license "Apache-2.0"

  on_macos do
    on_arm do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube_${VERSION}_darwin_arm64.tar.gz"
      sha256 "${DARWIN_ARM}"
    end
    on_intel do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube_${VERSION}_darwin_amd64.tar.gz"
      sha256 "${DARWIN_AMD}"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube_${VERSION}_linux_arm64.tar.gz"
      sha256 "${LINUX_ARM}"
    end
    on_intel do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube_${VERSION}_linux_amd64.tar.gz"
      sha256 "${LINUX_AMD}"
    end
  end

  def install
    bin.install "llmkube"
  end

  test do
    assert_match "llmkube", shell_output("#{bin}/llmkube version 2>&1")
  end
end

class LlmkubeForemanAgent < Formula
  desc "Foreman fleet-agent worker for off-cluster LLMKube nodes"
  homepage "https://github.com/defilantech/LLMKube"
  version "${VERSION}"
  license "Apache-2.0"

  on_macos do
    on_arm do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube-foreman-agent_${VERSION}_darwin_arm64.tar.gz"
      sha256 "${FOREMAN_DARWIN_ARM}"
    end
    on_intel do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube-foreman-agent_${VERSION}_darwin_amd64.tar.gz"
      sha256 "${FOREMAN_DARWIN_AMD}"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube-foreman-agent_${VERSION}_linux_arm64.tar.gz"
      sha256 "${FOREMAN_LINUX_ARM}"
    end
    on_intel do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube-foreman-agent_${VERSION}_linux_amd64.tar.gz"
      sha256 "${FOREMAN_LINUX_AMD}"
    end
  end

  def install
    bin.install "foreman-agent"
  end

  def plist_path
    etc/"launchd-load.d/llmkube-foreman-agent.plist"
  end

  def install
    bin.install "foreman-agent"
    # Ship a generic launchd plist that points at the Homebrew Cellar path.
    # Per-node flags (--fleet-node-name, --accelerator, --installed-models,
    # --kubeconfig, --git-remote-url, --commit-author-email) are expected to
    # be set by the operator in a companion env file at
    # /etc/llmkube/foreman-agent.env (one KEY=VALUE per line, lines starting
    # with '#' are comments). The launchd service sources that file via
    # EnvironmentVariables so upgrades do not drop operator-set flags.
    #
    # Operators who prefer to keep using the managed layout
    # (~/Library/Application Support/llmkube/foreman-agent) can skip this
    # plist and use `make install-foreman-agent` instead; the two paths are
    # mutually exclusive on a given node.
    plist_content = <<~PLIST
      <?xml version="1.0" encoding="UTF-8"?>
      <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
      <plist version="1.0">
      <dict>
          <key>Label</key>
          <string>com.llmkube.foreman-agent</string>
          <key>ProgramArguments</key>
          <array>
              <string>#{opt_bin}/foreman-agent</string>
              <string>--roles</string>
              <string>worker</string>
              <string>--agent-mode</string>
              <string>native</string>
              <string>--workspace-dir</string>
              <string>/tmp/foreman-workspaces</string>
              <string>--task-namespace</string>
              <string>default</string>
              <string>--foreman-namespace</string>
              <string>foreman-system</string>
          </array>
          <key>RunAtLoad</key>
          <true/>
          <key>KeepAlive</key>
          <true/>
          <key>StandardOutPath</key>
          <string>/tmp/llmkube-foreman-agent.log</string>
          <key>StandardErrorPath</key>
          <string>/tmp/llmkube-foreman-agent.log</string>
          <key>EnvironmentVariables</key>
          <dict>
              <key>PATH</key>
              <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
              <key>FOREMAN_AGENT_ENV_FILE</key>
              <string>/etc/llmkube/foreman-agent.env</string>
          </dict>
          <key>WorkingDirectory</key>
          <string>/tmp</string>
          <key>ThrottleInterval</key>
          <integer>30</integer>
      </dict>
      </plist>
    PLIST
    (etc/"launchd-load.d").mkpath
    (etc/"launchd-load.d/llmkube-foreman-agent.plist").write(plist_content)
  end

  def post_install
    # Ensure the env-file directory exists with sensible defaults. The agent
    # treats a missing env file as "no extra flags" and runs with its
    # built-in defaults, so this is purely a convenience for first-time
    # operators.
    env_dir = Pathname.new("/etc/llmkube")
    env_dir.mkpath unless env_dir.exist?
    env_file = env_dir/"foreman-agent.env"
    env_file.write("# Per-node foreman-agent flags, one KEY=VALUE per line.\n") if !env_file.exist?
  end

  def caveats
    <<~EOS
      The foreman-agent is installed as a launchd service. Per-node flags
      (node name, accelerator, installed models, kubeconfig, git remote URL,
      commit author email) live in /etc/llmkube/foreman-agent.env. Edit that
      file and run:

          brew services restart llmkube-foreman-agent

      To uninstall:

          brew services stop llmkube-foreman-agent
          brew uninstall llmkube-foreman-agent
    EOS
  end

  test do
    assert_match "foreman-agent", shell_output("#{bin}/foreman-agent --version 2>&1")
  end
end

class LlmkubeMetalAgent < Formula
  desc "Metal GPU agent for local LLM inference on Apple Silicon"
  homepage "https://github.com/defilantech/LLMKube"
  version "${VERSION}"
  license "Apache-2.0"

  on_macos do
    on_arm do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube-metal-agent_${VERSION}_darwin_arm64.tar.gz"
      sha256 "${METAL_DARWIN_ARM}"
    end
    on_intel do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube-metal-agent_${VERSION}_darwin_amd64.tar.gz"
      sha256 "${METAL_DARWIN_AMD}"
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
  render
  exit 0
fi

: "${HOMEBREW_TAP_TOKEN:?HOMEBREW_TAP_TOKEN is required to push the tap}"
workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT
git clone --depth 1 "https://x-access-token:${HOMEBREW_TAP_TOKEN}@github.com/${TAP_SLUG}.git" "$workdir/tap"

mkdir -p "$workdir/tap/Formula"
render >"$workdir/tap/Formula/llmkube.rb"
# Retire the macOS-only cask; the cross-platform formula supersedes it.
git -C "$workdir/tap" rm -f --ignore-unmatch Casks/llmkube.rb
git -C "$workdir/tap" add Formula/llmkube.rb

if git -C "$workdir/tap" diff --cached --quiet; then
  echo "llmkube formula already at v${VERSION}; nothing to publish"
  exit 0
fi

git -C "$workdir/tap" \
  -c user.name="github-actions[bot]" \
  -c user.email="41898282+github-actions[bot]@users.noreply.github.com" \
  commit -m "Update llmkube to v${VERSION} (cross-platform formula)"
git -C "$workdir/tap" push
echo "published llmkube formula v${VERSION} to ${TAP_SLUG}"
