package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/streamingfast/bstream/stream"
	"github.com/streamingfast/substreams/block"
	pbssinternal "github.com/streamingfast/substreams/pb/sf/substreams/intern/v2"
	"github.com/streamingfast/substreams/reqctx"
	"github.com/streamingfast/substreams/tracking"
	"go.uber.org/zap"
)

// OnStreamTerminated performs flush of store and setting trailers when the stream terminated gracefully from our point of view.
// If the stream terminated gracefully, we return `nil` otherwise, the original is returned.
func (p *Pipeline) OnStreamTerminated(ctx context.Context, err error) error {
	logger := reqctx.Logger(ctx)
	reqDetails := reqctx.Details(ctx)
	bytesMeter := tracking.GetBytesMeter(ctx)

	for _, executor := range p.moduleExecutors {
		executor.FreeMem()
	}

	if !errors.Is(err, stream.ErrStopBlockReached) && !errors.Is(err, io.EOF) {
		return err
	}

	logger.Info("stream of blocks ended",
		zap.Uint64("stop_block_num", reqDetails.StopBlockNum),
		zap.Bool("eof", errors.Is(err, io.EOF)),
		zap.Bool("stop_block_reached", errors.Is(err, stream.ErrStopBlockReached)),
		//zap.Uint64("total_bytes_written", bytesMeter.BytesWritten()),
		//zap.Uint64("total_bytes_read", bytesMeter.BytesRead()),
	)

	// TODO(abourget): check, in the tier1, there might not be a `lastFinalClock`
	// if we just didn't run the `streamFactoryFunc`
	if err := p.execOutputCache.EndOfStream(p.lastFinalClock); err != nil {
		return fmt.Errorf("end of stream: %w", err)
	}

	if err := p.stores.flushStores(ctx, reqDetails.StopBlockNum); err != nil {
		return fmt.Errorf("step new irr: stores end of stream: %w", err)
	}

	p.execOutputCache.Close()

	if err := bytesMeter.Send(ctx, p.respFunc); err != nil {
		return fmt.Errorf("sending bytes meter %w", err)
	}

	if p.stores.partialsWritten != nil {
		p.respFunc(&pbssinternal.ProcessRangeResponse{
			ModuleName: reqDetails.OutputModule,
			Type: &pbssinternal.ProcessRangeResponse_Completed{
				Completed: &pbssinternal.Completed{
					AllProcessedRanges: toPBInternalBlockRanges(p.stores.partialsWritten),
				},
			},
		})
	}

	return nil
}

func toPBInternalBlockRanges(in block.Ranges) (out []*pbssinternal.BlockRange) {
	for _, r := range in {
		out = append(out, &pbssinternal.BlockRange{
			StartBlock: r.StartBlock,
			EndBlock:   r.ExclusiveEndBlock,
		})
	}
	return
}
