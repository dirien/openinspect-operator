// runtimegen renders generated artifacts of the canonical runtime bundle
// (internal/runtime): the release Dockerfile and the digest. It exists so
// `make generate-runtime` and CI can prove the committed Dockerfile.runtime
// matches the manifest.
package main

import (
	"flag"
	"fmt"
	"os"

	sandboxruntime "github.com/dirien/openinspect-operator/internal/runtime"
)

func main() {
	out := flag.String("out", "Dockerfile.runtime", "path to write the rendered Dockerfile ('-' for stdout)")
	digestOnly := flag.Bool("digest", false, "print the runtime digest instead of writing the Dockerfile")
	flag.Parse()

	m, err := sandboxruntime.Load()
	if err != nil {
		fatal(err)
	}
	if *digestOnly {
		d, err := m.Digest()
		if err != nil {
			fatal(err)
		}
		fmt.Println(d)
		return
	}
	df, err := m.Dockerfile()
	if err != nil {
		fatal(err)
	}
	if *out == "-" {
		fmt.Print(df)
		return
	}
	if err := os.WriteFile(*out, []byte(df), 0o644); err != nil { //nolint:gosec // a Dockerfile is public build input
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s (upstream %s)\n", *out, m.UpstreamCommit)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "runtimegen:", err)
	os.Exit(1)
}
