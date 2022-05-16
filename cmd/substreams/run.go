package main

import (
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"os"
	"strconv"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/jhump/protoreflect/desc/protoparse"
	statikfs "github.com/rakyll/statik/fs"
	"github.com/spf13/cobra"
	"github.com/streamingfast/substreams/client"
	"github.com/streamingfast/substreams/decode"
	"github.com/streamingfast/substreams/manifest"
	pbsubstreams "github.com/streamingfast/substreams/pb/sf/substreams/v1"
	_ "github.com/streamingfast/substreams/pb/statik"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"go.uber.org/zap"
	"google.golang.org/grpc/metadata"
)

func init() {
	runCmd.Flags().StringP("substreams-endpoint", "e", "api.streamingfast.io:443", "Substreams gRPC endpoint")
	runCmd.Flags().String("substreams-api-token-envvar", "SUBSTREAMS_API_TOKEN", "name of variable containing Substreams Authentication token (JWT)")
	runCmd.Flags().Int64P("start-block", "s", -1, "Start block for blockchain firehose")
	runCmd.Flags().StringP("stop-block", "t", "0", "Stop block for blockchain firehose")
	runCmd.Flags().StringArrayP("proto-path", "I", []string{"./proto"}, "Import paths for protobuf schemas")
	runCmd.Flags().StringArray("proto", []string{"**/*.proto"}, "Path to explicit proto files (within proto-paths)")

	runCmd.Flags().BoolP("insecure", "k", false, "Skip certificate validation on GRPC connection")
	runCmd.Flags().BoolP("plaintext", "p", false, "Establish GRPC connection in plaintext")

	runCmd.Flags().BoolP("compact-output", "c", false, "Avoid pretty printing output for module and make it a single compact line")
	runCmd.Flags().Bool("partial-mode", false, "Request partial processing mode (internal deployments only)")
	runCmd.Flags().Bool("no-return-handler", false, "Avoid printing output for module")

	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(packCmd)

	packCmd.Flags().StringArrayP("proto-path", "I", []string{"./proto"}, "Import paths for protobuf schemas")
	packCmd.Flags().StringArray("proto", []string{"**/*.proto"}, "Path to explicit proto files (within proto-paths)")
}

// runCmd represents the command to run substreams remotely
var runCmd = &cobra.Command{
	Use:          "run <manifest> <module_name>",
	Short:        "Run substreams remotely",
	RunE:         run,
	Args:         cobra.ExactArgs(2),
	SilenceUsage: true,
}

var packCmd = &cobra.Command{
	Use:   "pack <manifest>",
	Short: "create a package from a manifest",
	RunE:  pack,
	Args:  cobra.ExactArgs(1),
}

func readFromStatik(filename string) ([]byte, error) {
	sfs, err := statikfs.New()
	if err != nil {
		return nil, err
	}
	staticFDS, err := sfs.Open(filename)
	if err != nil {
		return nil, err
	}
	b, err := ioutil.ReadAll(staticFDS)
	if err != nil {
		return nil, err
	}

	return b, nil
}

func pack(cmd *cobra.Command, args []string) error {
	fmt.Println("warning - not reading manifest for now, not implemented")

	protoImportPaths := mustGetStringArray(cmd, "proto-path")
	protoFilesPatterns := mustGetStringArray(cmd, "proto")
	protoFiles, err := findProtoFiles(protoImportPaths, protoFilesPatterns)
	if err != nil {
		return fmt.Errorf("finding proto files: %w", err)
	}
	parser := protoparse.Parser{
		ImportPaths:           protoImportPaths,
		IncludeSourceCodeInfo: true,
	}
	fileDescs, err := parser.ParseFiles(protoFiles...)
	if err != nil {
		return fmt.Errorf("error parsing proto files %q: %w", protoFiles, err)
	}

	fds := &descriptorpb.FileDescriptorSet{}

	b, err := readFromStatik("/system.pb")
	if err != nil {
		return err
	}
	_ = b
	//	err = proto.Unmarshal(b, fds)
	//	if err != nil {
	//		return err
	//	}
	//
	for _, fd := range fileDescs {
		fds.File = append(fds.File, fd.AsFileDescriptorProto())
	}
	seenFDS := make(map[string]bool)

	out := &descriptorpb.FileDescriptorSet{}
	for _, file := range fds.File {
		s := *file.Package
		for _, mt := range file.MessageType {
			s += *mt.Name
		}
		if seenFDS[s] {
			continue
		}
		out.File = append(out.File, file)
		fmt.Printf("bundling protobuf file %q\n", *file.Name)
		seenFDS[s] = true
	}

	b, err = proto.Marshal(out)
	if err != nil {
		return err
	}

	filename := "bundle.spkg"
	fmt.Printf("Writing file: %q\n", filename)

	fmt.Println(`You will need to create a file 'buf.gen.yaml' with this content:

version: v1
plugins:
  - name: prost
    out: gen/src

And then, run 'buf generate /path/to/bundle.spkg#format=bin'`)

	return ioutil.WriteFile(filename, b, fs.ModePerm)

}

func run(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	manifestPath := args[0]
	manif, err := manifest.New(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest %q: %w", manifestPath, err)
	}

	outputStreamNames := strings.Split(args[1], ",")

	protoImportPaths := mustGetStringArray(cmd, "proto-path")
	protoFilesPatterns := mustGetStringArray(cmd, "proto")
	protoFiles, err := findProtoFiles(protoImportPaths, protoFilesPatterns)
	if err != nil {
		return fmt.Errorf("finding proto files: %w", err)
	}
	parser := protoparse.Parser{
		ImportPaths: protoImportPaths,
	}
	fileDescs, err := parser.ParseFiles(protoFiles...)
	if err != nil {
		return fmt.Errorf("error parsing proto files %q: %w", protoFiles, err)
	}

	returnHandler := func(any *pbsubstreams.BlockScopedData, progress *pbsubstreams.ModulesProgress) error { return nil }
	if !mustGetBool(cmd, "no-return-handler") {
		returnHandler = decode.NewPrintReturnHandler(manif, fileDescs, outputStreamNames, !mustGetBool(cmd, "compact-output"))
	}

	failureProgressHandler := func(progress *pbsubstreams.ModulesProgress) error {
		failedModule := firstFailedModuleProgress(progress)
		if failedModule == nil {
			return nil
		}

		fmt.Printf("---------------------- Module %s failed ---------------------\n", failedModule.Name)
		for _, module := range progress.Modules {
			for _, log := range module.FailureLogs {
				fmt.Printf("%s: %s\n", module.Name, log)
			}

			if module.FailureLogsTruncated {
				fmt.Println("<Logs Truncated>")
			}
		}

		fmt.Printf("Error:\n%s", failedModule.FailureReason)
		return nil
	}

	manifProto, err := manif.ToProto()
	if err != nil {
		return fmt.Errorf("parse manifest to proto %q: %w", manifestPath, err)
	}

	graph, err := manifest.NewModuleGraph(manifProto.Modules)
	if err != nil {
		return fmt.Errorf("create module graph %w", err)
	}

	startBlock := mustGetInt64(cmd, "start-block")
	if startBlock == -1 {
		sb, err := graph.ModuleStartBlock(outputStreamNames[0])
		if err != nil {
			return fmt.Errorf("getting module start block: %w", err)
		}
		startBlock = int64(sb)
	}

	ssClient, callOpts, err := client.NewSubstreamsClient(
		mustGetString(cmd, "substreams-endpoint"),
		readAPIToken(cmd, "substreams-api-token-envvar"),
		mustGetBool(cmd, "insecure"),
		mustGetBool(cmd, "plaintext"),
	)
	if err != nil {
		return fmt.Errorf("substreams client setup: %w", err)
	}

	stopBlock, err := readStopBlockFlag(cmd, startBlock, "stop-block")
	if err != nil {
		return fmt.Errorf("stop block: %w", err)
	}

	req := &pbsubstreams.Request{
		StartBlockNum: startBlock,
		StopBlockNum:  stopBlock,
		ForkSteps:     []pbsubstreams.ForkStep{pbsubstreams.ForkStep_STEP_IRREVERSIBLE},
		Manifest:      manifProto,
		OutputModules: outputStreamNames,
	}

	if mustGetBool(cmd, "partial-mode") {
		ctx = metadata.NewOutgoingContext(ctx, metadata.New(map[string]string{"substreams-partial-mode": "true"}))
	}

	zlog.Info("connecting...")
	cli, err := ssClient.Blocks(ctx, req, callOpts...)
	if err != nil {
		return fmt.Errorf("call sf.substreams.v1.Stream/Blocks: %w", err)
	}

	zlog.Info("connected")

	for {
		resp, err := cli.Recv()
		if err != nil {
			if err == io.EOF {
				return nil
			}

			return err
		}

		switch r := resp.Message.(type) {
		case *pbsubstreams.Response_Progress:
			if failedModule := firstFailedModuleProgress(r.Progress); failedModule != nil {
				if err := failureProgressHandler(r.Progress); err != nil {
					fmt.Printf("FAILURE PROGRESS HANDLER ERROR: %s\n", err)
				}
			}
		case *pbsubstreams.Response_SnapshotData:
			_ = r.SnapshotData
		case *pbsubstreams.Response_SnapshotComplete:
			_ = r.SnapshotComplete
		case *pbsubstreams.Response_Data:
			if err := returnHandler(r.Data, nil); err != nil {
				fmt.Printf("RETURN HANDLER ERROR: %s\n", err)
			}
		}
	}
}

func firstFailedModuleProgress(modulesProgress *pbsubstreams.ModulesProgress) *pbsubstreams.ModuleProgress {
	for _, module := range modulesProgress.Modules {
		if module.Failed == true {
			return module
		}
	}

	return nil
}

func findProtoFiles(importPaths []string, importFilePatterns []string) ([]string, error) {
	var files []string
	for _, importPath := range importPaths {
		importPathFS := os.DirFS(importPath)
		for _, importFile := range importFilePatterns {
			zlog.Debug("globbing proto files", zap.String("import_path", importPath), zap.String("import_file", importFile))
			matches, err := doublestar.Glob(importPathFS, importFile)
			if err != nil {
				return nil, fmt.Errorf("glob through %q, matching %q: %w", importPath, importFile, err)
			}
			files = append(files, matches...)
		}
	}

	zlog.Debug("proto files found", zap.Strings("files", files))
	return files, nil
}

func readAPIToken(cmd *cobra.Command, envFlagName string) string {
	envVar := mustGetString(cmd, envFlagName)
	value := os.Getenv(envVar)
	if value != "" {
		return value
	}

	return os.Getenv("SF_API_TOKEN")
}

func readStopBlockFlag(cmd *cobra.Command, startBlock int64, flagName string) (uint64, error) {
	val, err := cmd.Flags().GetString(flagName)
	if err != nil {
		panic(fmt.Sprintf("flags: couldn't find flag %q", flagName))
	}

	isRelative := strings.HasPrefix(val, "+")
	if isRelative {
		if startBlock == -1 {
			return 0, fmt.Errorf("relative end block is supported only with an absolute start block")
		}

		val = strings.TrimPrefix(val, "+")
	}

	endBlock, err := strconv.ParseUint(val, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("end block is invalid: %w", err)
	}

	if isRelative {
		return uint64(startBlock) + endBlock, nil
	}

	return endBlock, nil
}
