#!/usr/bin/env bash
# Render Formula/llmkube-foreman-agent.rb and Formula/llmkube-metal-agent.rb
# from the release checksums and publish them to the Homebrew tap.
#
# Why separate formulae: the CLI is cross-platform (Linux/macOS/Windows) and
# ships one binary. The agents are macOS-only (Metal requires Apple Silicon)
# and carry per-node runtime config (--fleet-node-name, --accelerator,
# --installed-models, etc.) that a generic brew services plist cannot carry.
# Each agent gets its own formula with a dedicated plist template that sources
# an env file from ~/.config/llmkube/<agent>/env, so per-node flags survive
# `brew upgrade`.
#
# Usage: publish-homebrew-agent-formula.sh <version> [checksums-file]
#   DRY_RUN=1 prints the rendered formulae and exits (no clone, no push).
set -euo pipefail

VERSION="${1:?usage: publish-homebrew-agent-formula.sh <version> [checksums-file]}"
CHECKSUMS="${2:-dist/checksums.txt}"
TAP_SLUG="defilantech/homebrew-tap"

# sha_for prints the sha256 of the agent archive for the given os_arch, reading
# GoReleaser's "<sha256>  <filename>" checksums file.
sha_for() {
  local agent="$1" arch="$2" line
  line=$(grep -E "[[:space:]]LLMKube-${agent}_${VERSION}_${arch}\.tar\.gz\$" "$CHECKSUMS" || true)
  [ -n "$line" ] || { echo "no checksum for LLMKube-${agent}_${VERSION}_${arch}.tar.gz in $CHECKSUMS" >&2; exit 1; }
  echo "${line%% *}"
}

DARWIN_ARM=$(sha_for foreman-agent darwin_arm64)
DARWIN_AMD=$(sha_for foreman-agent darwin_amd64)
METAL_ARM=$(sha_for metal-agent darwin_arm64)
METAL_AMD=$(sha_for metal-agent darwin_amd64)

render_foreman_agent() {
  cat <<EOF
class LlmkubeForemanAgent < Formula
  desc "Foreman fleet-node agent for off-cluster Macs"
  homepage "https://github.com/defilantech/LLMKube"
  version "${VERSION}"
  license "Apache-2.0"

  on_macos do
    on_arm do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube-foreman-agent_${VERSION}_darwin_arm64.tar.gz"
      sha256 "${DARWIN_ARM}"
    end
    on_intel do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube-foreman-agent_${VERSION}_darwin_amd64.tar.gz"
      sha256 "${DARWIN_AMD}"
    end
  end

  def install
    bin.install "foreman-agent"

    # Ship the env-file-to-args wrapper. Operators edit
    # ~/.config/llmkube/foreman-agent/env (one --flag=value per line) and the
    # wrapper translates it to command-line args before execing the agent.
    # This keeps per-node config in a plain text file that survives `brew
    # upgrade`, unlike a hand-edited plist.
    libexec.install "deployment/macos/scripts/foreman-agent-env-to-args.sh" => "foreman-agent-env-to-args.sh"
    chmod 0755, libexec/"foreman-agent-env-to-args.sh"
  end

  def service
    OpenStruct.new(
      run: [opt_libexec/"foreman-agent-env-to-args.sh"],
      keep_alive: true,
      working_directory: HOMEBREW_PREFIX
    )
  end

  def caveats
    <<~EOS
      The foreman-agent runs as a launchd service managed by `brew services`.

      To configure it, create an env file with one --flag=value per line:

        mkdir -p ~/.config/llmkube/foreman-agent
        cat > ~/.config/llmkube/foreman-agent/env << ENV
      # Per-node flags, one per line as --flag=value or --flag value.
      # Required:
      --fleet-node-name=your-node-name
      --accelerator=metal
      --installed-models=model1,model2
      --kubeconfig=$HOME/.kube/config
      --git-remote-url=https://github.com/your/repo.git
      --commit-author-email=you@example.com
      ENV

      Then start the service:

        brew services start llmkube-foreman-agent

      To stop:

        brew services stop llmkube-foreman-agent

      To uninstall (stops service and removes binary):

        brew uninstall llmkube-foreman-agent
    EOS
  end

  test do
    assert_match "foreman-agent", shell_output("#{bin}/foreman-agent version 2>&1")
  end
end
EOF
}

render_metal_agent() {
  cat <<EOF
class LlmkubeMetalAgent < Formula
  desc "Metal GPU agent for local LLM inference on macOS"
  homepage "https://github.com/defilantech/LLMKube"
  version "${VERSION}"
  license "Apache-2.0"

  on_macos do
    on_arm do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube-metal-agent_${VERSION}_darwin_arm64.tar.gz"
      sha256 "${METAL_ARM}"
    end
    on_intel do
      url "https://github.com/defilantech/LLMKube/releases/download/v${VERSION}/LLMKube-metal-agent_${VERSION}_darwin_amd64.tar.gz"
      sha256 "${METAL_AMD}"
    end
  end

  def install
    bin.install "llmkube-metal-agent"

    # Ship the env-file-to-args wrapper. Operators edit
    # ~/.config/llmkube/metal-agent/env (one --flag=value per line) and the
    # wrapper translates it to command-line args before execing the agent.
    # This keeps per-node config in a plain text file that survives `brew
    # upgrade`, unlike a hand-edited plist.
    libexec.install "deployment/macos/scripts/metal-agent-env-to-args.sh" => "metal-agent-env-to-args.sh"
    chmod 0755, libexec/"metal-agent-env-to-args.sh"
  end

  def service
    OpenStruct.new(
      run: [opt_libexec/"metal-agent-env-to-args.sh"],
      keep_alive: true,
      working_directory: HOMEBREW_PREFIX
    )
  end

  def caveats
    <<~EOS
      The metal-agent runs as a launchd service managed by `brew services`.

      To configure it, create an env file with one --flag=value per line:

        mkdir -p ~/.config/llmkube/metal-agent
        cat > ~/.config/llmkube/metal-agent/env << ENV
      # Per-node flags, one per line as --flag=value or --flag value.
      # Required:
      --namespace=default
      --model-store=$HOME/.llmkube/models
      --llama-server=/opt/homebrew/bin/llama-server
      --port=9090
      # Optional:
      # --host-ip=192.168.1.50
      # --memory-fraction=0.75
      ENV

      Then start the service:

        brew services start llmkube-metal-agent

      To stop:

        brew services stop llmkube-metal-agent

      To uninstall (stops service and removes binary):

        brew uninstall llmkube-metal-agent
    EOS
  end

  test do
    assert_match "llmkube-metal-agent", shell_output("#{bin}/llmkube-metal-agent version 2>&1")
  end
end
EOF
}

if [ "${DRY_RUN:-}" = "1" ]; then
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
render_foreman_agent >"$workdir/tap/Formula/llmkube-foreman-agent.rb"
render_metal_agent >"$workdir/tap/Formula/llmkube-metal-agent.rb"

git -C "$workdir/tap" add Formula/llmkube-foreman-agent.rb Formula/llmkube-metal-agent.rb

if git -C "$workdir/tap" diff --cached --quiet; then
  echo "agent formulae already at v${VERSION}; nothing to publish"
  exit 0
fi

git -C "$workdir/tap" \
  -c user.name="github-actions[bot]" \
  -c user.email="41898282+github-actions[bot]@users.noreply.github.com" \
  commit -m "Add llmkube-foreman-agent and llmkube-metal-agent formulae (v${VERSION})"
git -C "$workdir/tap" push
echo "published agent formulae v${VERSION} to ${TAP_SLUG}"