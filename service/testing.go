package service

import (
	"context"
	"fmt"

	"github.com/streamingfast/substreams"

	pbssinternal "github.com/streamingfast/substreams/pb/sf/substreams/intern/v2"
	pbsubstreamsrpc "github.com/streamingfast/substreams/pb/sf/substreams/rpc/v2"
	"github.com/streamingfast/substreams/service/config"
)

func TestNewService(runtimeConfig config.RuntimeConfig, linearHandoffBlockNum uint64, streamFactoryFunc StreamFactoryFunc) *Tier1Service {
	return &Tier1Service{
		blockType:         "sf.substreams.v1.test.Block",
		streamFactoryFunc: streamFactoryFunc,
		runtimeConfig:     runtimeConfig,
		getRecentFinalBlock: func() (uint64, error) {
			if linearHandoffBlockNum != 0 {
				return linearHandoffBlockNum, nil
			}
			return 0, fmt.Errorf("no live feed")
		},
		tracer: nil,
		logger: zlog,
	}
}

func (s *Tier1Service) TestBlocks(ctx context.Context, isSubRequest bool, request *pbsubstreamsrpc.Request, respFunc substreams.ResponseFunc) error {
	return s.blocks(ctx, s.runtimeConfig, request, respFunc)
}

func TestNewServiceTier2(runtimeConfig config.RuntimeConfig, streamFactoryFunc StreamFactoryFunc) *Tier2Service {
	return &Tier2Service{
		blockType:         "sf.substreams.v1.test.Block",
		streamFactoryFunc: streamFactoryFunc,
		runtimeConfig:     runtimeConfig,
		tracer:            nil,
		logger:            zlog,
	}
}

func (s *Tier2Service) TestBlocks(ctx context.Context, request *pbssinternal.ProcessRangeRequest, respFunc substreams.ResponseFunc) error {
	return s.processRange(ctx, s.runtimeConfig, request, respFunc)
}
