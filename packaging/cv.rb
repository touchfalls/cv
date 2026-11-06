# Шаблон формулы для touchfalls/homebrew-tap. Версию и sha256 подставляет release.sh.
class Cv < Formula
  desc "Convert big videos (MKV/MP4 remux, compress, trim, join) and photos on macOS"
  homepage "https://github.com/touchfalls/cv"
  url "https://github.com/touchfalls/cv/archive/refs/tags/v@VERSION@.tar.gz"
  sha256 "@SHA256@"
  license "PolyForm-Noncommercial-1.0.0"

  depends_on "go" => :build
  depends_on "ffmpeg"
  depends_on :macos
  depends_on "webp"

  def install
    system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=#{version}")
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/cv -version")
  end
end
