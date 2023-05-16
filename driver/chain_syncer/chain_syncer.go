package chainSyncer

import (
	"context"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/taikoxyz/taiko-client/driver/chain_syncer/beaconsync"
	"github.com/taikoxyz/taiko-client/driver/chain_syncer/calldata"
	"github.com/taikoxyz/taiko-client/driver/state"
	"github.com/taikoxyz/taiko-client/pkg/rpc"
)

// L2ChainSyncer is responsible for keeping the L2 execution engine's local chain in sync with the one
// in TaikoL1 contract.
type L2ChainSyncer struct {
	ctx   context.Context
	state *state.State // Driver's state
	rpc   *rpc.Client  // L1/L2 RPC clients

	// Syncers
	beaconSyncer   *beaconsync.Syncer
	calldataSyncer *calldata.Syncer

	// Monitors
	progressTracker *beaconsync.SyncProgressTracker

	// If this flag is activated, will try P2P beacon sync if current node is behind of the protocol's
	// latest verified block head
	p2pSyncVerifiedBlocks bool
}

// New creates a new chain syncer instance.
func New(
	ctx context.Context,
	rpc *rpc.Client,
	state *state.State,
	p2pSyncVerifiedBlocks bool,
	p2pSyncTimeout time.Duration,
	signalServiceAddress common.Address,
) (*L2ChainSyncer, error) {
	tracker := beaconsync.NewSyncProgressTracker(rpc.L2, p2pSyncTimeout)
	go tracker.Track(ctx)

	beaconSyncer := beaconsync.NewSyncer(ctx, rpc, state, tracker)
	calldataSyncer, err := calldata.NewSyncer(ctx, rpc, state, tracker, signalServiceAddress)
	if err != nil {
		return nil, err
	}

	return &L2ChainSyncer{
		ctx:                   ctx,
		rpc:                   rpc,
		state:                 state,
		beaconSyncer:          beaconSyncer,
		calldataSyncer:        calldataSyncer,
		progressTracker:       tracker,
		p2pSyncVerifiedBlocks: p2pSyncVerifiedBlocks,
	}, nil
}

// Sync performs a sync operation to L2 execution engine's local chain.
func (s *L2ChainSyncer) Sync(l1End *types.Header) error {
	// If current L2 execution engine's chain is behind of the protocol's latest verified block head, and the
	// `P2PSyncVerifiedBlocks` flag is set, try triggering a beacon sync in L2 execution engine to catch up the
	// latest verified block head.
	if s.needNewBeaconSyncTriggered() {
		if err := s.beaconSyncer.TriggerBeaconSync(); err != nil {
			return fmt.Errorf("trigger beacon sync error: %w", err)
		}

		return nil
	}

	// We have triggered at least a beacon sync in L2 execution engine, we should reset the L1Current
	// cursor at first, before start inserting pending L2 blocks one by one.
	if s.progressTracker.Triggered() {
		log.Info(
			"Switch to insert pending blocks one by one",
			"p2pEnabled", s.p2pSyncVerifiedBlocks,
			"p2pOutOfSync", s.progressTracker.OutOfSync(),
		)

		// Get the execution engine's chain head.
		l2Head, err := s.rpc.L2.HeaderByNumber(s.ctx, nil)
		if err != nil {
			return err
		}

		// Make sure the execution engine's chain head is recorded in protocol.
		l2HeadHash, err := s.rpc.TaikoL1.GetCrossChainBlockHash(nil, l2Head.Number)
		if err != nil {
			return err
		}

		heightOrID := &state.HeightOrID{Height: l2Head.Number}
		// If there is a verified block hash mismatch, log the error and then try to re-sync from genesis one by one.
		if l2Head.Hash() != l2HeadHash {
			log.Error(
				"L2 block hash mismatch, re-sync from genesis",
				"height", l2Head.Number,
				"hash in protocol", common.Hash(l2HeadHash),
				"hash in execution engine", l2Head.Hash(),
			)

			heightOrID.ID = common.Big0
			heightOrID.Height = common.Big0
			if l2HeadHash, err = s.rpc.TaikoL1.GetCrossChainBlockHash(nil, common.Big0); err != nil {
				return err
			}
		}

		// If the L2 execution engine has synced to latest verified block.
		if l2HeadHash == s.progressTracker.LastSyncedVerifiedBlockHash() {
			heightOrID.ID = s.progressTracker.LastSyncedVerifiedBlockID()
		}

		// Reset the L1Current cursor.
		_, blockID, err := s.state.ResetL1Current(s.ctx, heightOrID)
		if err != nil {
			return err
		}

		// Reset to the latest L2 execution engine's chain status.
		s.progressTracker.UpdateMeta(blockID, heightOrID.Height, l2HeadHash)
	}

	// Insert the proposed block one by one.
	return s.calldataSyncer.ProcessL1Blocks(s.ctx, l1End)
}

// AheadOfProtocolVerifiedHead checks whether the L2 chain is ahead of verified head in protocol.
func (s *L2ChainSyncer) AheadOfProtocolVerifiedHead() bool {
	verifiedHeightToCompare := s.state.GetLatestVerifiedBlock().Height.Uint64()
	log.Debug(
		"Checking whether the execution engine is ahead of protocol's verified head",
		"latestVerifiedBlock", verifiedHeightToCompare,
		"executionEngineHead", s.state.GetL2Head().Number,
	)
	if verifiedHeightToCompare > 0 {
		// If latest verified head height is equal to L2 execution engine's synced head height minus one,
		// we also mark the triggered P2P sync progress as finished to prevent a potential `InsertBlockWithoutSetHead` in
		// execution engine, which may cause errors since we do not pass all transactions in ExecutePayload when calling
		// NewPayloadV1.
		verifiedHeightToCompare -= 1
	}

	if s.state.GetL2Head().Number.Uint64() < verifiedHeightToCompare {
		return false
	}

	if s.progressTracker.LastSyncedVerifiedBlockHeight() != nil {
		return s.state.GetL2Head().Number.Uint64() >= s.progressTracker.LastSyncedVerifiedBlockHeight().Uint64()
	}

	return true
}

// needNewBeaconSyncTriggered checks wthether the current L2 execution engine needs to trigger
// another new beacon sync.
func (s *L2ChainSyncer) needNewBeaconSyncTriggered() bool {
	return s.p2pSyncVerifiedBlocks &&
		s.state.GetLatestVerifiedBlock().Height.Uint64() > 0 &&
		!s.AheadOfProtocolVerifiedHead() &&
		!s.progressTracker.OutOfSync()
}

// BeaconSyncer returns the inner beacon syncer.
func (s *L2ChainSyncer) BeaconSyncer() *beaconsync.Syncer {
	return s.beaconSyncer
}

// CalldataSyncer returns the inner calldata syncer.
func (s *L2ChainSyncer) CalldataSyncer() *calldata.Syncer {
	return s.calldataSyncer
}
