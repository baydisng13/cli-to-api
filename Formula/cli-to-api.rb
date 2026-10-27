# cli-to-api.rb — Homebrew formula TEMPLATE for the baydisng13/homebrew-tap tap.
#
# This file does NOT live here to be installed from. It is the source template
# that gets copied into the separate tap repository as:
#
#     baydisng13/homebrew-tap :: Formula/cli-to-api.rb
#
# so that users can run:
#
#     brew install baydisng13/tap/cli-to-api
#
# It is a *binary* formula: there is no bottle and nothing is compiled. Each
# platform/arch pair downloads the matching archive published by
# .github/workflows/release.yml and installs the single `cli-to-api` file out
# of it.
#
# The `version` below is the release tag WITHOUT the leading "v" (Homebrew
# versions are bare semver); the URLs re-add the "v" because that is what the
# release pipeline names its assets:
#
#     cli-to-api_v<version>_<os>_<arch>.tar.gz
#
# The sha256 values come from the release's checksums.txt. Regenerate this
# whole file for a new release with:
#
#     packaging/homebrew/update-formula.sh v0.2.0
#
# See docs/PACKAGING.md.
class CliToApi < Formula
  desc "Serve local agentic CLIs behind an OpenAI-compatible API"
  homepage "https://github.com/baydisng13/cli-to-api"
  version "0.1.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/baydisng13/cli-to-api/releases/download/v#{version}/cli-to-api_v#{version}_darwin_arm64.tar.gz"
      sha256 "47925e0fb0b8b93e9a14b8f68a71d46e1c953eb08e0664d468512cd22b3df127"
    end
    on_intel do
      url "https://github.com/baydisng13/cli-to-api/releases/download/v#{version}/cli-to-api_v#{version}_darwin_amd64.tar.gz"
      sha256 "fcaa9de67e9a35ae585f79a018843339941937f7a76db2b68617dcbb38e3b1ca"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/baydisng13/cli-to-api/releases/download/v#{version}/cli-to-api_v#{version}_linux_arm64.tar.gz"
      sha256 "ece78837bde3abc196c12a4578ce763e7520946b9edd5155f5310357649bb0cd"
    end
    on_intel do
      url "https://github.com/baydisng13/cli-to-api/releases/download/v#{version}/cli-to-api_v#{version}_linux_amd64.tar.gz"
      sha256 "55eaa5c0ffa87df8a9176ca4af5f204a4d4237331e632547e31cc1606e64ac7e"
    end
  end

  # Tracks the newest GitHub release tag, so `brew livecheck cli-to-api` (and
  # Homebrew's own bump jobs) can tell when this formula is behind.
  livecheck do
    url :stable
    strategy :github_latest
  end

  def install
    # Every archive holds exactly one file at its root: `cli-to-api`.
    bin.install "cli-to-api"
  end

  def caveats
    <<~EOS
      Create your first API key with:
        cli-to-api key create

      Then run the server:
        cli-to-api start

      Config lives in $CLI_TO_API_HOME, else $XDG_CONFIG_HOME/cli-to-api,
      else ~/.config/cli-to-api
    EOS
  end

  test do
    # The release build injects the tag (e.g. "v0.1.0") via
    # -ldflags "-X main.version=...", so the binary prints the tag with its
    # leading "v" while `version` here is the bare "0.1.0". Substring match
    # covers both, and still fails if the wrong build got installed.
    assert_match version.to_s, shell_output("#{bin}/cli-to-api version")

    # `help` goes to stdout and exits 0; proves the binary actually runs on
    # this machine rather than just existing.
    assert_match "OpenAI-compatible API", shell_output("#{bin}/cli-to-api help")

    # An unknown subcommand must fail loudly rather than silently succeeding.
    assert_match "unknown command", shell_output("#{bin}/cli-to-api not-a-command 2>&1", 1)
  end
end
