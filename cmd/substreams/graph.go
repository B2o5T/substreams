package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/streamingfast/cli"

	"github.com/streamingfast/substreams/manifest"
	"github.com/streamingfast/substreams/tools"
)

var graphCmd = &cobra.Command{
	Use:   "graph [<manifest_file>]",
	Short: "Generate mermaid-js graph document",
	RunE:  runManifestGraph,
	Long: cli.Dedent(`
		Generate mermaid-js graph document. The manifest is optional as it will try to find a file named
		'substreams.yaml' in current working directory if nothing entered. You may enter a directory that contains a
		'substreams.yaml' file in place of '<manifest_file>'.
	`),
	Args:         cobra.RangeArgs(0, 1),
	SilenceUsage: true,
}

func init() {
	rootCmd.AddCommand(graphCmd)
}

func runManifestGraph(cmd *cobra.Command, args []string) error {
	manifestPathRaw := ""
	if len(args) == 1 {
		manifestPathRaw = args[0]
	}
	manifestPath, err := tools.ResolveManifestFile(manifestPathRaw)
	if err != nil {
		return fmt.Errorf("resolving manifest: %w", err)
	}

	manifestReader := manifest.NewReader(manifestPath)
	pkg, err := manifestReader.Read()
	if err != nil {
		return fmt.Errorf("read manifest %q: %w", manifestPath, err)
	}

	manifest.PrintMermaid(pkg.Modules)

	fmt.Println("")
	fmt.Println("Here is a quick link to see the graph:")
	fmt.Println("")
	fmt.Println(manifest.GenerateMermaidLiveURL(pkg.Modules))

	return nil
}
