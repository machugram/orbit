class Orbit < Formula
  desc "MCP server for autonomous agent infrastructure on OrbStack"
  homepage "https://github.com/anthropics/orbit"
  version "0.1.0"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/anthropics/orbit/releases/download/v#{version}/orbit-aarch64-apple-darwin"
      sha256 "0000000000000000000000000000000000000000000000000000000000000000"
    else
      url "https://github.com/anthropics/orbit/releases/download/v#{version}/orbit-x86_64-apple-darwin"
      sha256 "1111111111111111111111111111111111111111111111111111111111111111"
    end
  end

  depends_on "orbstack"
  depends_on "docker"

  def install
    bin.install "orbit-#{Hardware::CPU.arch == :arm64 ? 'aarch64-apple-darwin' : 'x86_64-apple-darwin'}" => "orbit"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/orbit --version 2>&1", 1)
  end
end
