package main

import (
	"fmt"
	"github.com/streamingfast/substreams/tools"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/streamingfast/cli"
	"github.com/streamingfast/substreams/codegen"
	"github.com/streamingfast/substreams/manifest"
	"go.uber.org/zap"
)

var protogenCmd = &cobra.Command{
	Use:   "protogen [<package>]",
	Short: "GenerateProto Rust bindings from a package",
	Long: cli.Dedent(`
		GenerateProto Rust bindings from a package. The manifest is optional as it will try to find 
		one in your pwd if nothing entered. You may enter a dir that contains a 'substreams.yaml' file in place of <manifest_file>
	`),
	RunE:         runProtogen,
	Args:         cobra.RangeArgs(0, 1),
	SilenceUsage: true,
}

func init() {
	rootCmd.AddCommand(protogenCmd)
	protogenCmd.Flags().StringP("output-path", "o", "src/pb", cli.FlagDescription(`
		Directory to output generated .rs files, if the received <package> argument is a local Substreams manifest file
		(e.g. a local file ending with .yaml), the output path will be made relative to it
	`))
	protogenCmd.Flags().StringArrayP("exclude-paths", "x", []string{}, "Exclude specific files or directories, for example \"proto/a/a.proto\" or \"proto/a\"")
}

func runProtogen(cmd *cobra.Command, args []string) error {
	outputPath := mustGetString(cmd, "output-path")

	excludePaths := mustGetStringArray(cmd, "exclude-paths")

	var manifestPath string
	var err error

	if len(args) == 0 {
		manifestPath, err = tools.ResolveManifestFile("")
		if err != nil {
			return fmt.Errorf("resolving manifest: %w", err)
		}
	} else {
		manifestPath, err = tools.ResolveManifestFile(args[0])
		if err != nil {
			return fmt.Errorf("resolving manifest: %w", err)
		}
	}
	manifestReader := manifest.NewReader(manifestPath, manifest.SkipSourceCodeReader(), manifest.SkipModuleOutputTypeValidationReader())

	if manifestReader.IsLocalManifest() && !filepath.IsAbs(outputPath) {
		newOutputPath := filepath.Join(filepath.Dir(manifestPath), outputPath)

		zlog.Debug("manifest path is a local manifest, making output path relative to it", zap.String("old", outputPath), zap.String("new", newOutputPath))
		outputPath = newOutputPath
	}

	pkg, err := manifestReader.Read()
	if err != nil {
		return fmt.Errorf("reading manifest %q: %w", manifestPath, err)
	}

	// write the manifest to temp location
	// write buf.gen.yaml with custom stuff
	// run `buf generate`
	// remove if we wrote buf.gen.yaml (--keep-buf-gen-yaml)
	if _, err = manifest.NewModuleGraph(pkg.Modules.Modules); err != nil {
		return fmt.Errorf("processing module graph %w", err)
	}

	generator := codegen.NewProtoGenerator(outputPath, excludePaths)
	return generator.GenerateProto(pkg)
}
