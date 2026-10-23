// Command contractcheck verifies the frozen SDK2/UAPI contract without updating it.
//
// Usage from the module root:
//
//	go run ./internal/gen/contractcheck
//	go run ./internal/gen/contractcheck -source J:/tansr/tansr-cli
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/tansrai/tansr-go/internal/contractlock"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("contractcheck", flag.ContinueOnError)
	flags.SetOutput(stderr)
	contractDir := flags.String("contract", "contract", "directory containing frozen LOCK.json and copies")
	sourceDir := flags.String("source", "", "optional tansr-cli checkout to compare without changing it")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "contractcheck: unexpected positional argument")
		return 2
	}
	report, err := contractlock.Check(*contractDir, *sourceDir)
	if err != nil {
		fmt.Fprintf(stderr, "contractcheck: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s verified: revision=%d files=%d families=%d operations=%d schemaHash=%s sourceCommit=%s\n",
		report.Baseline, report.ManifestRevision, report.Files, report.Families, report.Operations, report.SchemaHash, report.SourceCommit)
	if report.SourceOnlyMetadata > 0 {
		fmt.Fprintf(stdout, "source-only metadata=%d; private source bytes verified=%t (without -source, only manifest/hash correspondence is checked)\n", report.SourceOnlyMetadata, report.SourceOnlyBytesVerified)
	}
	if report.SourceHEAD != "" {
		fmt.Fprintf(stdout, "source checkout verified: HEAD=%s (current public contracts and historical per-file Git references checked separately)\n", report.SourceHEAD)
	}
	return 0
}
