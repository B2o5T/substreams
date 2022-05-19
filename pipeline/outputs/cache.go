package outputs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/streamingfast/bstream"
	"github.com/streamingfast/dstore"
	"github.com/streamingfast/substreams/block"
	pbsubstreams "github.com/streamingfast/substreams/pb/sf/substreams/v1"
	"go.uber.org/zap"
)

type CacheItem struct {
	BlockNum uint64 `json:"block_num"`
	BlockID  string
	Payload  []byte `json:"payload"`
}
type outputKV map[string]*CacheItem
type OutputCache struct {
	lock sync.RWMutex

	ModuleName        string
	CurrentBlockRange *block.Range
	//kv                map[string]*bstream.Block
	kv                outputKV
	Store             dstore.Store
	New               bool
	saveBlockInterval uint64
}

type ModulesOutputCache struct {
	OutputCaches      map[string]*OutputCache
	SaveBlockInterval uint64
}

func NewModuleOutputCache(saveBlockInterval uint64) *ModulesOutputCache {
	zlog.Debug("creating cache with modules")
	moduleOutputCache := &ModulesOutputCache{
		OutputCaches:      make(map[string]*OutputCache),
		SaveBlockInterval: saveBlockInterval,
	}

	return moduleOutputCache
}

func (c *ModulesOutputCache) RegisterModule(ctx context.Context, module *pbsubstreams.Module, hash string, baseCacheStore dstore.Store, requestedStartBlock uint64) (*OutputCache, error) {
	zlog.Debug("registering modules", zap.String("module_name", module.Name))

	if cache, found := c.OutputCaches[module.Name]; found {
		return cache, nil
	}

	moduleStore, err := baseCacheStore.SubStore(fmt.Sprintf("%s/outputs", hash))
	if err != nil {
		return nil, fmt.Errorf("creating substore for module %q: %w", module.Name, err)
	}

	cache := NewOutputCache(module.Name, moduleStore, c.SaveBlockInterval)

	c.OutputCaches[module.Name] = cache

	return cache, nil
}

func (c *ModulesOutputCache) Update(ctx context.Context, blockRef bstream.BlockRef) error {
	for _, moduleCache := range c.OutputCaches {
		if !moduleCache.CurrentBlockRange.Contains(blockRef) {
			zlog.Debug("updating cache", zap.Stringer("block_ref", blockRef))
			if err := moduleCache.save(ctx); err != nil {
				return fmt.Errorf("saving blocks for module kv %s: %w", moduleCache.ModuleName, err)
			}
			if err := moduleCache.Load(ctx, moduleCache.CurrentBlockRange.ExclusiveEndBlock); err != nil {
				return fmt.Errorf("loading blocks for module kv %s: %w", moduleCache.ModuleName, err)
			}
		}
	}

	return nil
}

func (c *ModulesOutputCache) Save(ctx context.Context) error {
	zlog.Info("Saving caches")
	for _, moduleCache := range c.OutputCaches {
		if err := moduleCache.save(ctx); err != nil {
			return fmt.Errorf("save: saving outpust or module kv %s: %w", moduleCache.ModuleName, err)
		}
	}
	return nil
}

func NewOutputCache(moduleName string, store dstore.Store, saveBlockInterval uint64) *OutputCache {
	return &OutputCache{
		ModuleName:        moduleName,
		Store:             store,
		saveBlockInterval: saveBlockInterval,
	}
}

func (c *OutputCache) SortedCacheItem() (out []*CacheItem) {
	for _, item := range c.kv {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].BlockNum < out[j].BlockNum
	})
	return
}

func (c *OutputCache) Set(block *bstream.Block, data []byte) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	if !c.New {
		zlog.Warn("trying to add output to an already existing module kv", zap.String("module_name", c.ModuleName))
		return nil
	}

	//pbBlock := &bstream.Block{
	//	Id:             block.ID(),
	//	Number:         block.Num(),
	//	PreviousId:     block.PreviousID(),
	//	Timestamp:      block.Time(),
	//	LibNum:         block.LIBNum(),
	//	PayloadKind:    pbbstream.Protocol_UNKNOWN,
	//	PayloadVersion: int32(1),
	//}
	//
	//_, err := bstream.MemoryBlockPayloadSetter(pbBlock, data)
	//if err != nil {
	//	return fmt.Errorf("setting block payload for block %s: %w", block.Id, err)
	//}

	c.kv[block.Id] = &CacheItem{
		BlockNum: block.Num(),
		BlockID:  block.Id,
		Payload:  data,
	}

	return nil
	//c.lock.Lock()
	//defer c.lock.Unlock()
	//
	//if !c.new {
	//	zlog.Warn("trying to add output to an already existing module kv", zap.String("module_name", c.moduleName))
	//	return nil
	//}
	//
	//pbBlock := &bstream.Block{
	//	Id:             block.ID(),
	//	Number:         block.Num(),
	//	PreviousId:     block.PreviousID(),
	//	Timestamp:      block.Time(),
	//	LibNum:         block.LIBNum(),
	//	PayloadKind:    pbbstream.Protocol_UNKNOWN,
	//	PayloadVersion: int32(1),
	//}
	//
	//_, err := bstream.MemoryBlockPayloadSetter(pbBlock, data)
	//if err != nil {
	//	return fmt.Errorf("setting block payload for block %s: %w", block.Id, err)
	//}
	//
	//c.kv[block.Id] = pbBlock
	//
	//return nil
}

func (c *OutputCache) Get(block *bstream.Block) ([]byte, bool, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	cacheItem, found := c.kv[block.Id]

	if !found {
		return nil, false, nil
	}

	return cacheItem.Payload, found, nil
	//c.lock.Lock()
	//defer c.lock.Unlock()
	//
	//b, found := c.kv[block.Id]
	//
	//if !found {
	//	return nil, false, nil
	//}
	//
	//data, err := b.Payload.Get()
	//
	//return data, found, err

}

func (c *OutputCache) Load(ctx context.Context, atBlock uint64) (err error) {
	zlog.Info("loading outputs", zap.String("module_name", c.ModuleName), zap.Uint64("at_block_num", atBlock))

	c.New = false
	c.kv = make(outputKV)

	var found bool
	c.CurrentBlockRange, found, err = findBlockRange(ctx, c.Store, atBlock)
	if err != nil {
		return fmt.Errorf("computing block range for module %q: %w", c.ModuleName, err)
	}

	if !found {
		c.CurrentBlockRange = &block.Range{
			StartBlock:        atBlock,
			ExclusiveEndBlock: atBlock + c.saveBlockInterval,
		}

		c.New = true
		return nil
	}

	zlog.Debug("loading outputs data", zap.String("cache_module_name", c.ModuleName), zap.Object("block_range", c.CurrentBlockRange))

	filename := computeDBinFilename(pad(c.CurrentBlockRange.StartBlock), pad(c.CurrentBlockRange.ExclusiveEndBlock))
	objectReader, err := c.Store.OpenObject(ctx, filename)
	if err != nil {
		return fmt.Errorf("loading block reader %s: %w", filename, err)
	}

	err = json.NewDecoder(objectReader).Decode(&c.kv)
	if err != nil {
		return fmt.Errorf("json decoding file %s: %w", filename, err)
	}
	zlog.Debug("cache loaded", zap.String("cache_module_name", c.ModuleName), zap.Stringer("block_range", c.CurrentBlockRange))
	return nil

	//blockReader, err := bstream.GetBlockReaderFactory.New(objectReader)
	//if err != nil {
	//	return fmt.Errorf("getting block reader %s: %w", filename, err)
	//}
	//
	//for {
	//	block, err := blockReader.Read()
	//
	//	if err != nil && err != io.EOF {
	//		return fmt.Errorf("reading block: %w", err)
	//	}
	//
	//	if block == nil {
	//		return nil
	//	}
	//
	//	c.kv[block.Id] = block
	//
	//	if err == io.EOF {
	//		return nil
	//	}
	//}
}

func (c *OutputCache) save(ctx context.Context) error {
	zlog.Info("saving cache", zap.String("module_name", c.ModuleName), zap.Stringer("block_range", c.CurrentBlockRange))
	filename := computeDBinFilename(pad(c.CurrentBlockRange.StartBlock), pad(c.CurrentBlockRange.ExclusiveEndBlock))

	buffer := bytes.NewBuffer(nil)
	err := json.NewEncoder(buffer).Encode(c.kv)
	if err != nil {
		return fmt.Errorf("json encoding outputs: %w", err)
	}

	err = c.Store.WriteObject(ctx, filename, buffer)
	if err != nil {
		return fmt.Errorf("writing block buffer to store: %w", err)
	}
	zlog.Debug("cache saved", zap.String("module_name", c.ModuleName), zap.String("file_name", filename), zap.String("url", c.Store.BaseURL().String()))
	return nil
	//zlog.Info("saving cache", zap.String("module_name", c.moduleName), zap.Stringer("block_range", c.currentBlockRange))
	//filename := computeDBinFilename(pad(c.currentBlockRange.StartBlock), pad(c.currentBlockRange.ExclusiveEndBlock))
	//
	//buffer := bytes.NewBuffer(nil)
	//blockWriter, err := bstream.GetBlockWriterFactory.New(buffer)
	//if err != nil {
	//	return fmt.Errorf("write block factory: %w", err)
	//}
	//
	//for _, block := range c.kv {
	//	if err := blockWriter.Write(block); err != nil {
	//		return fmt.Errorf("write block: %w", err)
	//	}
	//}
	//
	//err = c.store.WriteObject(ctx, filename, buffer)
	//if err != nil {
	//	return fmt.Errorf("writing block buffer to store: %w", err)
	//}
	//
	//return nil
}

func findBlockRange(ctx context.Context, store dstore.Store, prefixStartBlock uint64) (*block.Range, bool, error) {
	var exclusiveEndBlock uint64

	paddedBlock := pad(prefixStartBlock)

	files, err := store.ListFiles(ctx, paddedBlock, ".tmp", math.MaxInt64)
	if err != nil {
		return nil, false, fmt.Errorf("walking prefix for padded block %s: %w", paddedBlock, err)
	}

	if len(files) == 0 {
		return nil, false, nil
	}

	biggestEndBlock := uint64(0)

	for _, file := range files {
		endBlock, err := getExclusiveEndBlock(file)
		if err != nil {
			return nil, false, fmt.Errorf("getting exclusive end block from file %s: %w", file, err)
		}
		if endBlock > biggestEndBlock {
			biggestEndBlock = endBlock
		}
	}

	exclusiveEndBlock = biggestEndBlock

	return &block.Range{
		StartBlock:        prefixStartBlock,
		ExclusiveEndBlock: exclusiveEndBlock,
	}, true, nil
}

func computeDBinFilename(startBlock, stopBlock string) string {
	return fmt.Sprintf("%s-%s.output", startBlock, stopBlock)
}

func pad(blockNumber uint64) string {
	return fmt.Sprintf("000%d", blockNumber)
}

func ComputeStartBlock(startBlock uint64, saveBlockInterval uint64) uint64 {
	return startBlock - startBlock%saveBlockInterval
}

func getExclusiveEndBlock(filename string) (uint64, error) {
	endBlock := strings.Split(filename, "-")[1]
	parsedInt, err := strconv.ParseInt(strings.TrimPrefix(strings.Split(endBlock, ".")[0], "000"), 10, 64)

	if err != nil {
		return 0, fmt.Errorf("parsing int %d: %w", parsedInt, err)
	}

	return uint64(parsedInt), nil
}