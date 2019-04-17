class Skaffold < Formula
  desc "A tool that facilitates continuous development for Kubernetes applications."
  head "https://github.com/apollocatlin/skaffold.git"
  url "https://github.com/apollocatlin/skaffold.git"

  depends_on "go" => :build

  def install
    ENV["GOPATH"] = buildpath
    (buildpath/"src/github.com/apollocatlin").mkpath

    ln_s buildpath, buildpath/"src/github.com/apollocatlin/skaffold"
    system "make"
    bin.install "out/skaffold"
  end

  test do
    system "#{bin}skaffold", "--version"
  end
end
