package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/streamingfast/substreams/pipeline/outputmodules"

	"github.com/streamingfast/bstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	ttrace "go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/streamingfast/substreams"
	"github.com/streamingfast/substreams/orchestrator"
	pbssinternal "github.com/streamingfast/substreams/pb/sf/substreams/intern/v2"
	pbsubstreamsrpc "github.com/streamingfast/substreams/pb/sf/substreams/rpc/v2"
	pbsubstreams "github.com/streamingfast/substreams/pb/sf/substreams/v1"
	"github.com/streamingfast/substreams/pipeline/cache"
	"github.com/streamingfast/substreams/pipeline/exec"
	"github.com/streamingfast/substreams/reqctx"
	"github.com/streamingfast/substreams/service/config"
	"github.com/streamingfast/substreams/storage/execout"
	"github.com/streamingfast/substreams/storage/store"
	"github.com/streamingfast/substreams/wasm"
)

type processingModule struct {
	name            string
	initialBlockNum uint64
}

type Pipeline struct {
	ctx           context.Context
	runtimeConfig config.RuntimeConfig

	pendingUndoMessage *pbsubstreamsrpc.Response
	preBlockHooks      []substreams.BlockHook
	postBlockHooks     []substreams.BlockHook
	postJobHooks       []substreams.PostJobHook

	wasmRuntime     *wasm.Runtime
	outputGraph     *outputmodules.Graph
	moduleExecutors []exec.ModuleExecutor

	mapModuleOutput         *pbsubstreamsrpc.MapModuleOutput
	extraMapModuleOutputs   []*pbsubstreamsrpc.MapModuleOutput
	extraStoreModuleOutputs []*pbsubstreamsrpc.StoreModuleOutput

	respFunc         func(substreams.ResponseFromAnyTier) error
	lastProgressSent time.Time

	stores         *Stores
	execoutStorage *execout.Configs

	processingModule *processingModule

	gate            *gate
	finalBlocksOnly bool

	forkHandler     *ForkHandler
	insideReorgUpTo bstream.BlockRef

	execOutputCache *cache.Engine

	// lastFinalClock should always be either THE `stopBlock` or a block beyond that point
	// (for chains with potential block skips)
	lastFinalClock *pbsubstreams.Clock
}

func New(
	ctx context.Context,
	outputGraph *outputmodules.Graph,
	stores *Stores,
	execoutStorage *execout.Configs,
	wasmRuntime *wasm.Runtime,
	execOutputCache *cache.Engine,
	runtimeConfig config.RuntimeConfig,
	respFunc func(substreams.ResponseFromAnyTier) error,
	opts ...Option,
) *Pipeline {
	pipe := &Pipeline{
		ctx:             ctx,
		gate:            newGate(ctx),
		execOutputCache: execOutputCache,
		runtimeConfig:   runtimeConfig,
		outputGraph:     outputGraph,
		wasmRuntime:     wasmRuntime,
		respFunc:        respFunc,
		stores:          stores,
		execoutStorage:  execoutStorage,
		forkHandler:     NewForkHandler(),
	}
	for _, opt := range opts {
		opt(pipe)
	}
	return pipe
}

func (p *Pipeline) InitStoresAndBackprocess(ctx context.Context) (err error) {
	reqDetails := reqctx.Details(ctx)
	logger := reqctx.Logger(ctx)
	ctx, span := reqctx.WithSpan(ctx, "pipeline_init")
	defer span.EndWithErr(&err)

	p.forkHandler.registerUndoHandler(func(clock *pbsubstreams.Clock, moduleOutputs []*pbssinternal.ModuleOutput) {
		for _, modOut := range moduleOutputs {
			p.stores.storesHandleUndo(modOut)
		}
	})

	p.setupProcessingModule(reqDetails)

	var storeMap store.Map
	if reqDetails.IsSubRequest {
		logger.Info("stores loaded", zap.Object("stores", p.stores.StoreMap))
		if storeMap, err = p.setupSubrequestStores(ctx); err != nil {
			return fmt.Errorf("failed to load stores: %w", err)
		}
	} else {
		if storeMap, err = p.runParallelProcess(ctx); err != nil {
			return fmt.Errorf("failed setup request: %w", err)
		}
	}
	p.stores.SetStoreMap(storeMap)

	return nil
}

func (p *Pipeline) InitWASM(ctx context.Context) (err error) {

	// TODO(abourget): Build the Module Executor list: this could be done lazily, but the outputmodules.Graph,
	//  and cache the latest if all block boundaries
	//  are still clear.

	return p.buildWASM(ctx, p.outputGraph.UsedModules())
}

func (p *Pipeline) GetStoreMap() store.Map {
	return p.stores.StoreMap
}

func (p *Pipeline) setupProcessingModule(reqDetails *reqctx.RequestDetails) {
	for _, module := range reqDetails.Modules.Modules {
		if reqDetails.IsOutputModule(module.Name) {
			p.processingModule = &processingModule{
				name:            module.GetName(),
				initialBlockNum: reqDetails.ResolvedStartBlockNum,
			}
		}
	}
}

func (p *Pipeline) setupSubrequestStores(ctx context.Context) (store.Map, error) {
	reqDetails := reqctx.Details(ctx)
	logger := reqctx.Logger(ctx)

	outputModuleName := reqDetails.OutputModule

	ttrace.SpanContextFromContext(context.Background())
	storeMap := store.NewMap()

	for name, storeConfig := range p.stores.configs {
		if name == outputModuleName {
			partialStore := storeConfig.NewPartialKV(reqDetails.ResolvedStartBlockNum, logger)
			storeMap.Set(partialStore)
		} else {
			fullStore := storeConfig.NewFullKV(logger)

			//fixme: should we check if we don't have a boundary finished to not load ?
			if fullStore.InitialBlock() != reqDetails.ResolvedStartBlockNum {
				if err := fullStore.Load(ctx, reqDetails.ResolvedStartBlockNum); err != nil {
					return nil, fmt.Errorf("load full store: %w", err)
				}
			}

			storeMap.Set(fullStore)
		}
	}

	return storeMap, nil
}

// runParallelProcess
func (p *Pipeline) runParallelProcess(ctx context.Context) (storeMap store.Map, err error) {
	ctx, span := reqctx.WithSpan(ctx, "parallelprocess")
	defer span.EndWithErr(&err)
	reqDetails := reqctx.Details(ctx)
	reqStats := reqctx.ReqStats(ctx)
	logger := reqctx.Logger(ctx)

	parallelProcessor, err := orchestrator.BuildParallelProcessor(
		p.ctx,
		reqDetails,
		p.runtimeConfig,
		p.outputGraph,
		p.execoutStorage,
		p.respFunc,
		p.stores.configs,
		p.pendingUndoMessage,
	)
	if err != nil {
		return nil, fmt.Errorf("building parallel processor: %w", err)
	}

	logger.Info("starting parallel processing")

	reqStats.StartParallelProcessing()
	storeMap, err = parallelProcessor.Run(ctx)
	if err != nil {
		return nil, fmt.Errorf("parallel processing run: %w", err)
	}
	reqStats.EndParallelProcessing()

	p.processingModule = nil

	return storeMap, nil
}

func (p *Pipeline) isOutputModule(name string) bool {
	return p.outputGraph.IsOutputModule(name)
}

func (p *Pipeline) runPostJobHooks(ctx context.Context, clock *pbsubstreams.Clock) {
	for _, hook := range p.postJobHooks {
		if err := hook(ctx, clock); err != nil {
			reqctx.Logger(ctx).Warn("post job hook failed", zap.Error(err))
		}
	}
}

func (p *Pipeline) runPreBlockHooks(ctx context.Context, clock *pbsubstreams.Clock) (err error) {
	_, span := reqctx.WithSpan(ctx, "pre_block_hooks")
	defer span.EndWithErr(&err)

	for _, hook := range p.preBlockHooks {
		span.AddEvent("running_pre_block_hook", ttrace.WithAttributes(attribute.String("hook", fmt.Sprintf("%T", hook))))
		if err := hook(ctx, clock); err != nil {
			return fmt.Errorf("pre block hook: %w", err)
		}
	}
	return nil
}

func (p *Pipeline) execute(ctx context.Context, executor exec.ModuleExecutor, execOutput execout.ExecutionOutput) (err error) {
	logger := reqctx.Logger(ctx)

	executor.ResetWASMCall()

	executorName := executor.Name()
	hasValidOutput := executor.HasValidOutput()
	logger.Debug("executing", zap.Uint64("block", execOutput.Clock().Number), zap.String("module_name", executorName))

	moduleOutput, outputBytes, runError := exec.RunModule(ctx, executor, execOutput)
	if runError != nil {
		if hasValidOutput {
			p.saveModuleOutput(moduleOutput, executor.Name(), reqctx.Details(ctx).ProductionMode)
		}
		return fmt.Errorf("execute module: %w", runError)
	}

	if !hasValidOutput {
		return nil
	}
	p.saveModuleOutput(moduleOutput, executor.Name(), reqctx.Details(ctx).ProductionMode)
	if err := execOutput.Set(executorName, outputBytes); err != nil {
		return fmt.Errorf("set output cache: %w", err)
	}
	if moduleOutput != nil {
		p.forkHandler.addReversibleOutput(moduleOutput, execOutput.Clock().Id)
	}
	return nil
}

func (p *Pipeline) saveModuleOutput(output *pbssinternal.ModuleOutput, moduleName string, isProduction bool) {
	if p.isOutputModule(moduleName) {
		p.mapModuleOutput = toRPCMapModuleOutputs(output)
		return
	}
	if isProduction {
		return
	}

	if storeOutputs := toRPCStoreModuleOutputs(output); storeOutputs != nil {
		p.extraStoreModuleOutputs = append(p.extraStoreModuleOutputs, storeOutputs)
		return
	}
	if mapOutput := toRPCMapModuleOutputs(output); mapOutput != nil {
		p.extraMapModuleOutputs = append(p.extraMapModuleOutputs, mapOutput)
	}
	return

}

func toRPCStoreModuleOutputs(in *pbssinternal.ModuleOutput) (out *pbsubstreamsrpc.StoreModuleOutput) {
	deltas := in.GetStoreDeltas()
	if deltas == nil {
		return nil
	}
	return &pbsubstreamsrpc.StoreModuleOutput{
		Name:             in.ModuleName,
		DebugStoreDeltas: toRPCDeltas(deltas),
		DebugInfo: &pbsubstreamsrpc.OutputDebugInfo{
			Logs:          in.Logs,
			LogsTruncated: in.DebugLogsTruncated,
			Cached:        in.Cached,
		},
	}
}

func toRPCDeltas(in *pbssinternal.StoreDeltas) (out []*pbsubstreamsrpc.StoreDelta) {
	for _, d := range in.StoreDeltas {
		out = append(out, &pbsubstreamsrpc.StoreDelta{
			Operation: toRPCOperation(d.Operation),
			Ordinal:   d.Ordinal,
			Key:       d.Key,
			OldValue:  d.OldValue,
			NewValue:  d.NewValue,
		})
	}
	return
}

func toRPCOperation(in pbssinternal.StoreDelta_Operation) (out pbsubstreamsrpc.StoreDelta_Operation) {
	switch in {
	case pbssinternal.StoreDelta_UPDATE:
		return pbsubstreamsrpc.StoreDelta_UPDATE
	case pbssinternal.StoreDelta_CREATE:
		return pbsubstreamsrpc.StoreDelta_CREATE
	case pbssinternal.StoreDelta_DELETE:
		return pbsubstreamsrpc.StoreDelta_DELETE
	}
	return pbsubstreamsrpc.StoreDelta_UNSET
}

func toRPCMapModuleOutputs(in *pbssinternal.ModuleOutput) (out *pbsubstreamsrpc.MapModuleOutput) {
	data := in.GetMapOutput()
	if data == nil {
		return nil
	}
	return &pbsubstreamsrpc.MapModuleOutput{
		Name:      in.ModuleName,
		MapOutput: data,
		DebugInfo: &pbsubstreamsrpc.OutputDebugInfo{
			Logs:          in.Logs,
			LogsTruncated: in.DebugLogsTruncated,
			Cached:        in.Cached,
		},
	}
}

func (p *Pipeline) returnRPCModuleProgressOutputs(clock *pbsubstreams.Clock) error {
	var progress []*pbsubstreamsrpc.ModuleProgress
	if p.processingModule != nil {
		progress = append(progress, &pbsubstreamsrpc.ModuleProgress{
			Name: p.processingModule.name,
			Type: &pbsubstreamsrpc.ModuleProgress_ProcessedRanges_{
				ProcessedRanges: &pbsubstreamsrpc.ModuleProgress_ProcessedRanges{
					ProcessedRanges: []*pbsubstreamsrpc.BlockRange{
						{
							StartBlock: p.processingModule.initialBlockNum,
							EndBlock:   clock.Number,
						},
					},
				},
			},
		})
	}
	if p.respFunc != nil {
		if err := p.respFunc(substreams.NewModulesProgressResponse(progress)); err != nil {
			return fmt.Errorf("calling return func: %w", err)
		}
	}
	return nil
}

func (p *Pipeline) returnInternalModuleProgressOutputs(clock *pbsubstreams.Clock, forceOutput bool) error {
	if p.respFunc != nil {
		if forceOutput || time.Since(p.lastProgressSent) > progressMessageInterval {
			p.lastProgressSent = time.Now()
			out := &pbssinternal.ProcessRangeResponse{
				ModuleName: p.processingModule.name,
				Type: &pbssinternal.ProcessRangeResponse_ProcessedRange{
					ProcessedRange: &pbssinternal.BlockRange{
						StartBlock: p.processingModule.initialBlockNum,
						EndBlock:   clock.Number,
					},
				},
			}

			if err := p.respFunc(out); err != nil {
				return fmt.Errorf("calling return func: %w", err)
			}
		}
	}
	return nil
}

// TODO(abourget): have this being generated and the `buildWASM` by taking
// this Graph as input, and creating the ModuleExecutors, and caching
// them over there.
// moduleExecutorsInitialized bool
// moduleExecutors            []exec.ModuleExecutor
func (p *Pipeline) buildWASM(ctx context.Context, modules []*pbsubstreams.Module) error {
	reqModules := reqctx.Details(ctx).Modules
	tracer := otel.GetTracerProvider().Tracer("executor")

	loadedModules := make(map[uint32]*wasm.Module)
	for _, module := range modules {
		if _, exists := loadedModules[module.BinaryIndex]; exists {
			continue
		}
		code := reqModules.Binaries[module.BinaryIndex]
		m, err := p.wasmRuntime.NewModule(code.Content)
		if err != nil {
			return fmt.Errorf("new wasm module: %w", err)
		}
		loadedModules[module.BinaryIndex] = m
	}

	for _, module := range modules {
		inputs, err := p.renderWasmInputs(module)
		if err != nil {
			return fmt.Errorf("module %q: get wasm inputs: %w", module.Name, err)
		}

		entrypoint := module.BinaryEntrypoint
		instance, err := p.wasmRuntime.NewInstance(ctx, loadedModules[module.BinaryIndex], module.Name, module.BinaryEntrypoint)
		if err != nil {
			return fmt.Errorf("new wasm module: %w", err)
		}

		switch kind := module.Kind.(type) {
		case *pbsubstreams.Module_KindMap_:
			outType := strings.TrimPrefix(module.Output.Type, "proto:")
			baseExecutor := exec.NewBaseExecutor(
				module.Name,
				instance,
				inputs,
				entrypoint,
				tracer,
			)
			executor := exec.NewMapperModuleExecutor(baseExecutor, outType)
			p.moduleExecutors = append(p.moduleExecutors, executor)

		case *pbsubstreams.Module_KindStore_:
			updatePolicy := kind.KindStore.UpdatePolicy
			valueType := kind.KindStore.ValueType

			outputStore, found := p.stores.StoreMap.Get(module.Name)
			if !found {
				return fmt.Errorf("store %q not found", module.Name)
			}
			inputs = append(inputs, wasm.NewStoreWriterOutput(module.Name, outputStore, updatePolicy, valueType))

			baseExecutor := exec.NewBaseExecutor(
				module.Name,
				instance,
				inputs,
				entrypoint,
				tracer,
			)
			executor := exec.NewStoreModuleExecutor(baseExecutor, outputStore)
			p.moduleExecutors = append(p.moduleExecutors, executor)

		default:
			panic(fmt.Errorf("invalid kind %q input module %q", module.Kind, module.Name))
		}
	}

	return nil
}

func returnModuleDataOutputs(
	clock *pbsubstreams.Clock,
	cursor *bstream.Cursor,
	mapModuleOutput *pbsubstreamsrpc.MapModuleOutput,
	extraMapModuleOutputs []*pbsubstreamsrpc.MapModuleOutput,
	extraStoreModuleOutputs []*pbsubstreamsrpc.StoreModuleOutput,
	respFunc func(substreams.ResponseFromAnyTier) error,
) error {
	out := &pbsubstreamsrpc.BlockScopedData{
		Clock:             clock,
		Output:            mapModuleOutput,
		DebugMapOutputs:   extraMapModuleOutputs,
		DebugStoreOutputs: extraStoreModuleOutputs,
		Cursor:            cursor.ToOpaque(),
		FinalBlockHeight:  cursor.LIB.Num(),
	}

	if err := respFunc(substreams.NewBlockScopedDataResponse(out)); err != nil {
		return fmt.Errorf("calling return func: %w", err)
	}

	return nil
}

func (p *Pipeline) renderWasmInputs(module *pbsubstreams.Module) (out []wasm.Argument, err error) {
	storeAccessor := p.stores.StoreMap
	for _, input := range module.Inputs {
		switch in := input.Input.(type) {
		case *pbsubstreams.Module_Input_Params_:
			out = append(out, wasm.NewParamsInput(input.GetParams().GetValue()))
		case *pbsubstreams.Module_Input_Map_:
			out = append(out, wasm.NewMapInput(in.Map.ModuleName))
		case *pbsubstreams.Module_Input_Store_:
			inputName := input.GetStore().ModuleName
			if input.GetStore().Mode == pbsubstreams.Module_Input_Store_DELTAS {
				out = append(out, wasm.NewMapInput(inputName))
			} else {
				inputStore, found := storeAccessor.Get(inputName)
				if !found {
					return nil, fmt.Errorf("store %q npt found", inputName)
				}
				out = append(out, wasm.NewStoreReaderInput(inputName, inputStore))
			}
		case *pbsubstreams.Module_Input_Source_:
			// in.Source.Type checking against `blockType` is already done
			// upfront in `validateGraph`.
			out = append(out, wasm.NewSourceInput(in.Source.Type))
		default:
			return nil, fmt.Errorf("invalid input struct for module %q", module.Name)
		}
	}
	return out, nil
}
