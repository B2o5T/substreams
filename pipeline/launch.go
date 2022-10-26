package pipeline

import (
	"context"
	"errors"
	"fmt"
	"github.com/streamingfast/bstream/stream"
	"github.com/streamingfast/substreams/reqctx"
	"go.uber.org/zap"
	"google.golang.org/grpc/metadata"
	"io"
	"strings"
)

type Streamable interface {
	Run(ctx context.Context) error
}

type Trailable interface {
	SetTrailer(metadata.MD)
}

func (p *Pipeline) Launch(ctx context.Context, stream Streamable, streamSrv Trailable) error {
	return p.onStreamTerminated(ctx, streamSrv, stream.Run(ctx))
}

// onStreamTerminated performs flush of store and setting trailers when the stream terminated gracefully from our point of view.
// If the stream terminated gracefully, we return `nil` otherwise, the original is returned.
func (p *Pipeline) onStreamTerminated(ctx context.Context, streamSrv Trailable, err error) error {
	logger := reqctx.Logger(ctx)
	reqDetails := reqctx.Details(ctx)

	if errors.Is(err, stream.ErrStopBlockReached) || errors.Is(err, io.EOF) {
		logger.Debug("stream of blocks ended",
			zap.Uint64("stop_block_num", reqDetails.Request.StopBlockNum),
			zap.Bool("eof", errors.Is(err, io.EOF)),
			zap.Bool("stop_block_reached", errors.Is(err, stream.ErrStopBlockReached)),
		)

		if err := p.execOutputCache.EndOfStream(reqDetails.Request.StopBlockNum); err != nil {
			return fmt.Errorf("step new irr: exec out end of stream: %w", err)
		}

		if err := p.flushStores(ctx, reqDetails.Request.StopBlockNum); err != nil {
			return fmt.Errorf("step new irr: stores end of stream: %w", err)
		}

		partialRanges := make([]string, len(p.partialsWritten))
		for i, rng := range p.partialsWritten {
			partialRanges[i] = fmt.Sprintf("%d-%d", rng.StartBlock, rng.ExclusiveEndBlock)
		}

		logger.Info("setting trailer", zap.Strings("ranges", partialRanges))
		streamSrv.SetTrailer(metadata.MD{"substreams-partials-written": []string{strings.Join(partialRanges, ",")}})

		// It was an ok error, so let's
		return nil
	}

	// We are not responsible for doing any other error handling here, caller will deal with them
	return err
}