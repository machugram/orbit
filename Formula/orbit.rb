# TEMPLATE — not installable as-is.
#
# Do not `brew install` this file until a GitHub Release exists and the sha256
# values below are replaced with the real checksums from that release.
class Orbit < Formula
  desc "MCP server for autonomous agent infrastructure on OrbStack"
  homepage "https://github.com/machugram/orbit"
  version "0.1.0"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/machugram/orbit/releases/download/v#{version}/orbit-aarch64-apple-darwin"
      sha256 "0000000000000000000000000000000000000000000000000000000000000000" # PLACEHOLDER — replace after release
    else
      url "https://github.com/machugram/orbit/releases/download/v#{version}/orbit-x86_64-apple-darwin"
      sha256 "1111111111111111111111111111111111111111111111111111111111111111" # PLACEHOLDER — replace after release
    end
  end

  depends_on "orbstack"
  depends_on "docker"

  def install
    bin.install "orbit-#{Hardware::CPU.arch == :arm64 ? 'aarch64-apple-darwin' : 'x86_64-apple-darwin'}" => "orbit"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/orbit --version")
  end
end
