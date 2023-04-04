package beaconsync

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/log"
	"github.com/taikoxyz/taiko-client/bindings/encoding"
	"github.com/taikoxyz/taiko-client/driver/state"
	"github.com/taikoxyz/taiko-client/pkg/rpc"
)

// Syncer responsible for letting the L2 execution engine catching up with protocol's latest
// verified block through P2P beacon sync.
type Syncer struct {
	ctx             context.Context
	rpc             *rpc.Client
	state           *state.State
	progressTracker *SyncProgressTracker // Sync progress tracker
}

// NewSyncer creates a new syncer instance.
func NewSyncer(
	ctx context.Context,
	rpc *rpc.Client,
	state *state.State,
	progressTracker *SyncProgressTracker,
) *Syncer {
	return &Syncer{ctx, rpc, state, progressTracker}
}

// TriggerBeaconSync triggers the L2 execution engine to start performing a beacon sync.
func (s *Syncer) TriggerBeaconSync() error {
	blockID, latestVerifiedHeadPayload, err := s.getVerifiedBlockPayload(s.ctx)
	if err != nil {
		return err
	}

	if !s.progressTracker.HeadChanged(blockID) {
		log.Debug("Verified head has not changed", "blockID", blockID, "hash", latestVerifiedHeadPayload.BlockHash)
		return nil
	}

	status, err := s.rpc.L2Engine.NewPayload(
		s.ctx,
		latestVerifiedHeadPayload,
	)
	if err != nil {
		return err
	}

	if status.Status != engine.SYNCING && status.Status != engine.VALID {
		return fmt.Errorf("unexpected NewPayload response status: %s", status.Status)
	}

	fcRes, err := s.rpc.L2Engine.ForkchoiceUpdate(s.ctx, &engine.ForkchoiceStateV1{
		HeadBlockHash:      latestVerifiedHeadPayload.BlockHash,
		SafeBlockHash:      latestVerifiedHeadPayload.BlockHash,
		FinalizedBlockHash: latestVerifiedHeadPayload.BlockHash,
	}, nil)
	if err != nil {
		return err
	}
	if fcRes.PayloadStatus.Status != engine.SYNCING {
		return fmt.Errorf("unexpected ForkchoiceUpdate response status: %s", status.Status)
	}

	// Update sync status.
	s.progressTracker.UpdateMeta(
		blockID,
		new(big.Int).SetUint64(latestVerifiedHeadPayload.Number),
		latestVerifiedHeadPayload.BlockHash,
	)

	log.Info(
		"⛓️ Beacon sync triggered",
		"newHeadID", blockID,
		"newHeadHeight", s.progressTracker.LastSyncedVerifiedBlockHeight(),
		"newHeadHash", s.progressTracker.LastSyncedVerifiedBlockHash(),
	)

	return nil
}

// getVerifiedBlockPayload fetches the latest verified block's header, and converts it to an Engine API executable data,
// which will be used to let the node to start beacon syncing.
func (s *Syncer) getVerifiedBlockPayload(ctx context.Context) (*big.Int, *engine.ExecutableData, error) {
	var (
		latestVerifiedBlock = s.state.GetLatestVerifiedBlock()
	)

	header, err := s.rpc.L2CheckPoint.HeaderByNumber(s.ctx, latestVerifiedBlock.ID)
	if err != nil {
		return nil, nil, err
	}

	if header.Hash() != latestVerifiedBlock.Hash {
		return nil, nil, fmt.Errorf(
			"latest verified block hash mismatch: %s != %s", header.Hash(), latestVerifiedBlock.Hash,
		)
	}

	log.Info("Latest verified block header retrieved", "hash", header.Hash())

	return latestVerifiedBlock.ID, encoding.ToExecutableDataV1(header), nil
}
