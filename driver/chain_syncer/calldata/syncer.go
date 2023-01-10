package calldata

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/beacon"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/taikoxyz/taiko-client/bindings"
	anchorTxConstructor "github.com/taikoxyz/taiko-client/driver/anchor_tx_constructor"
	progressTracker "github.com/taikoxyz/taiko-client/driver/chain_syncer/progress_tracker"
	"github.com/taikoxyz/taiko-client/driver/state"
	"github.com/taikoxyz/taiko-client/metrics"
	eventIterator "github.com/taikoxyz/taiko-client/pkg/chain_iterator/event_iterator"
	"github.com/taikoxyz/taiko-client/pkg/rpc"
	txListValidator "github.com/taikoxyz/taiko-client/pkg/tx_list_validator"
)

// Syncer responsible for letting the L2 execution engine catching up with protocol's latest
// pending block through deriving L1 calldata.
type Syncer struct {
	ctx                           context.Context
	rpc                           *rpc.Client
	state                         *state.State
	progressTracker               *progressTracker.BeaconSyncProgressTracker // Sync progress tracker
	anchorConstructor             *anchorTxConstructor.AnchorTxConstructor   // TaikoL2.anchor transactions constructor
	txListValidator               *txListValidator.TxListValidator           // Transactions list validator
	throwawayBlocksBuilderPrivKey *ecdsa.PrivateKey                          // Private key of L2 throwaway blocks builder
	// Used by BlockInserter
	lastInsertedBlockID *big.Int
}

// NewSyncer creates a new syncer instance.
func NewSyncer(
	ctx context.Context,
	rpc *rpc.Client,
	state *state.State,
	progressTracker *progressTracker.BeaconSyncProgressTracker,
	throwawayBlocksBuilderPrivKey *ecdsa.PrivateKey, // Private key of L2 throwaway blocks builder
) (*Syncer, error) {
	configs, err := rpc.TaikoL1.GetConfig(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get protocol configs: %w", err)
	}

	constructor, err := anchorTxConstructor.New(
		rpc,
		configs.AnchorTxGasLimit.Uint64(),
		bindings.GoldenTouchAddress,
		bindings.GoldenTouchPrivKey,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize anchor constructor: %w", err)
	}

	return &Syncer{
		ctx:               ctx,
		rpc:               rpc,
		state:             state,
		progressTracker:   progressTracker,
		anchorConstructor: constructor,
		txListValidator: txListValidator.NewTxListValidator(
			configs.BlockMaxGasLimit.Uint64(),
			configs.MaxTransactionsPerBlock.Uint64(),
			configs.MaxBytesPerTxList.Uint64(),
			configs.MinTxGasLimit.Uint64(),
			rpc.L2ChainID,
		),
		throwawayBlocksBuilderPrivKey: throwawayBlocksBuilderPrivKey,
	}, nil
}

// ProcessL1Blocks fetches all `TaikoL1.BlockProposed` events between given
// L1 block heights, and then tries inserting them into L2 execution engine's block chain.
func (s *Syncer) ProcessL1Blocks(ctx context.Context, l1End *types.Header) error {
	iter, err := eventIterator.NewBlockProposedIterator(ctx, &eventIterator.BlockProposedIteratorConfig{
		Client:               s.rpc.L1,
		TaikoL1:              s.rpc.TaikoL1,
		StartHeight:          s.state.GetL1Current().Number,
		EndHeight:            l1End.Number,
		FilterQuery:          nil,
		OnBlockProposedEvent: s.onBlockProposed,
	})
	if err != nil {
		return err
	}

	if err := iter.Iter(); err != nil {
		return err
	}

	s.state.SetL1Current(l1End)
	metrics.DriverL1CurrentHeightGauge.Update(s.state.GetL1Current().Number.Int64())

	return nil
}

// OnBlockProposed is a `BlockProposed` event callback which responsible for
// inserting the proposed block one by one to the L2 execution engine.
func (s *Syncer) onBlockProposed(
	ctx context.Context,
	event *bindings.TaikoL1ClientBlockProposed,
	endIter eventIterator.EndBlockProposedEventIterFunc,
) error {
	// Ignore those already inserted blocks.
	if event.Id.Cmp(common.Big0) == 0 || (s.lastInsertedBlockID != nil && event.Id.Cmp(s.lastInsertedBlockID) <= 0) {
		return nil
	}

	log.Info(
		"New BlockProposed event",
		"L1Height", event.Raw.BlockNumber,
		"L1Hash", event.Raw.BlockHash,
		"BlockID", event.Id,
	)

	// Fetch the L2 parent block.
	var (
		parent *types.Header
		err    error
	)
	if s.progressTracker.Triggered() {
		// Already synced through beacon sync, just skip this event.
		if event.Id.Cmp(s.progressTracker.LastSyncedVerifiedBlockID()) <= 0 {
			return nil
		}

		parent, err = s.rpc.L2.HeaderByHash(ctx, s.progressTracker.LastSyncedVerifiedBlockHash())
	} else {
		parent, err = s.rpc.L2ParentByBlockId(ctx, event.Id)
	}

	if err != nil {
		return fmt.Errorf("failed to fetch L2 parent block: %w", err)
	}

	log.Debug("Parent block", "height", parent.Number, "hash", parent.Hash())

	tx, err := s.rpc.L1.TransactionInBlock(
		ctx,
		event.Raw.BlockHash,
		event.Raw.TxIndex,
	)
	if err != nil {
		return fmt.Errorf("failed to fetch original TaikoL1.proposeBlock transaction: %w", err)
	}

	// Check whether the transactions list is valid.
	txListBytes, hint, invalidTxIndex, err := s.txListValidator.ValidateTxList(event.Id, tx.Data())
	if err != nil {
		return fmt.Errorf("failed to validate transactions list: %w", err)
	}

	log.Info(
		"Validate transactions list",
		"blockID", event.Id,
		"hint", hint,
		"invalidTxIndex", invalidTxIndex,
	)

	l1Origin := &rawdb.L1Origin{
		BlockID:       event.Id,
		L2BlockHash:   common.Hash{}, // Will be set by taiko-geth.
		L1BlockHeight: new(big.Int).SetUint64(event.Raw.BlockNumber),
		L1BlockHash:   event.Raw.BlockHash,
		Throwaway:     hint != txListValidator.HintOK,
	}

	if event.Meta.Timestamp > uint64(time.Now().Unix()) {
		log.Warn("Future L2 block, waiting", "L2 block timestamp", event.Meta.Timestamp, "now", time.Now().Unix())
		time.Sleep(time.Until(time.Unix(int64(event.Meta.Timestamp), 0)))
	}

	var (
		payloadData  *beacon.ExecutableDataV1
		rpcError     error
		payloadError error
	)
	if hint == txListValidator.HintOK {
		payloadData, rpcError, payloadError = s.insertNewHead(
			ctx,
			event,
			parent,
			s.state.GetHeadBlockID(),
			txListBytes,
			l1Origin,
		)
	} else {
		payloadData, rpcError, payloadError = s.insertThrowAwayBlock(
			ctx,
			event,
			parent,
			uint8(hint),
			new(big.Int).SetInt64(int64(invalidTxIndex)),
			s.state.GetHeadBlockID(),
			txListBytes,
			l1Origin,
		)
	}

	// RPC errors are recoverable.
	if rpcError != nil {
		return fmt.Errorf("failed to insert new head to L2 execution engine: %w", rpcError)
	}

	if payloadError != nil {
		log.Warn(
			"Ignore invalid block context", "blockID", event.Id, "payloadError", payloadError, "payloadData", payloadData,
		)
		return nil
	}

	log.Debug("Payload data", "hash", payloadData.BlockHash, "txs", len(payloadData.Transactions))

	log.Info(
		"🔗 New L2 block inserted",
		"throwaway", l1Origin.Throwaway,
		"blockID", event.Id,
		"height", payloadData.Number,
		"hash", payloadData.BlockHash,
		"latestVerifiedBlockHeight", s.state.GetLatestVerifiedBlock().Height,
		"latestVerifiedBlockHash", s.state.GetLatestVerifiedBlock().Hash,
		"transactions", len(payloadData.Transactions),
	)

	metrics.DriverL1CurrentHeightGauge.Update(int64(event.Raw.BlockNumber))
	s.lastInsertedBlockID = event.Id

	if !l1Origin.Throwaway && s.progressTracker.Triggered() {
		s.progressTracker.ClearMeta()
	}

	return nil
}

// insertNewHead tries to insert a new head block to the L2 execution engine's local
// block chain through Engine APIs.
func (s *Syncer) insertNewHead(
	ctx context.Context,
	event *bindings.TaikoL1ClientBlockProposed,
	parent *types.Header,
	headBlockID *big.Int,
	txListBytes []byte,
	l1Origin *rawdb.L1Origin,
) (*beacon.ExecutableDataV1, error, error) {
	log.Debug(
		"Try to insert a new L2 head block",
		"parentNumber", parent.Number,
		"parentHash", parent.Hash(),
		"headBlockID", headBlockID,
		"l1Origin", l1Origin,
	)

	// Insert a TaikoL2.anchor transaction at transactions list head
	var txList []*types.Transaction
	if len(txListBytes) != 0 {
		if err := rlp.DecodeBytes(txListBytes, &txList); err != nil {
			log.Info("Ignore invalid txList bytes", "blockID", event.Id)
			return nil, nil, err
		}
	}

	// Assemble a TaikoL2.anchor transaction
	anchorTx, err := s.anchorConstructor.AssembleAnchorTx(
		ctx,
		event.Meta.L1Height,
		event.Meta.L1Hash,
		parent.Number,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create TaikoL2.anchor transaction: %w", err)
	}

	txList = append([]*types.Transaction{anchorTx}, txList...)

	if txListBytes, err = rlp.EncodeToBytes(txList); err != nil {
		log.Warn("Encode txList error", "blockID", event.Id, "error", err)
		return nil, nil, err
	}

	payload, rpcErr, payloadErr := s.createExecutionPayloads(
		ctx,
		event,
		parent.Hash(),
		l1Origin,
		headBlockID,
		txListBytes,
	)

	if rpcErr != nil || payloadErr != nil {
		return nil, rpcErr, payloadErr
	}

	fc := &beacon.ForkchoiceStateV1{HeadBlockHash: parent.Hash()}

	// Update the fork choice
	fc.HeadBlockHash = payload.BlockHash
	fcRes, err := s.rpc.L2Engine.ForkchoiceUpdate(ctx, fc, nil)
	if err != nil {
		return nil, err, nil
	}
	if fcRes.PayloadStatus.Status != beacon.VALID {
		return nil, nil, fmt.Errorf("unexpected ForkchoiceUpdate response status: %s", fcRes.PayloadStatus.Status)
	}

	return payload, nil, nil
}

// insertThrowAwayBlock tries to insert a throw away block to the L2 execution engine's local
// block chain through Engine APIs.
func (s *Syncer) insertThrowAwayBlock(
	ctx context.Context,
	event *bindings.TaikoL1ClientBlockProposed,
	parent *types.Header,
	hint uint8,
	invalidTxIndex *big.Int,
	headBlockID *big.Int,
	txListBytes []byte,
	l1Origin *rawdb.L1Origin,
) (*beacon.ExecutableDataV1, error, error) {
	log.Debug(
		"Try to insert a new L2 throwaway block",
		"parentHash", parent.Hash(),
		"headBlockID", headBlockID,
		"l1Origin", l1Origin,
	)

	// Assemble a TaikoL2.invalidateBlock transaction
	opts, err := s.getInvalidateBlockTxOpts(ctx, parent.Number)
	if err != nil {
		return nil, nil, err
	}

	invalidateBlockTx, err := s.rpc.TaikoL2.InvalidateBlock(
		opts,
		txListBytes,
		hint,
		invalidTxIndex,
	)
	if err != nil {
		return nil, nil, err
	}

	throwawayBlockTxListBytes, err := rlp.EncodeToBytes(
		types.Transactions{invalidateBlockTx},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to encode TaikoL2.InvalidateBlock transaction bytes, err: %w", err)
	}

	return s.createExecutionPayloads(
		ctx,
		event,
		parent.Hash(),
		l1Origin,
		headBlockID,
		throwawayBlockTxListBytes,
	)
}

// createExecutionPayloads creates a new execution payloads through
// Engine APIs.
func (s *Syncer) createExecutionPayloads(
	ctx context.Context,
	event *bindings.TaikoL1ClientBlockProposed,
	parentHash common.Hash,
	l1Origin *rawdb.L1Origin,
	headBlockID *big.Int,
	txListBytes []byte,
) (payloadData *beacon.ExecutableDataV1, rpcError error, payloadError error) {
	fc := &beacon.ForkchoiceStateV1{HeadBlockHash: parentHash}
	attributes := &beacon.PayloadAttributesV1{
		Timestamp:             event.Meta.Timestamp,
		Random:                event.Meta.MixHash,
		SuggestedFeeRecipient: event.Meta.Beneficiary,
		BlockMetadata: &beacon.BlockMetadata{
			HighestBlockID: headBlockID,
			Beneficiary:    event.Meta.Beneficiary,
			GasLimit:       event.Meta.GasLimit + s.anchorConstructor.GasLimit(),
			Timestamp:      event.Meta.Timestamp,
			TxList:         txListBytes,
			MixHash:        event.Meta.MixHash,
			ExtraData:      event.Meta.ExtraData,
		},
		L1Origin: l1Origin,
	}

	// Step 1, prepare a payload
	fcRes, err := s.rpc.L2Engine.ForkchoiceUpdate(ctx, fc, attributes)
	if err != nil {
		return nil, err, nil
	}
	if fcRes.PayloadStatus.Status != beacon.VALID {
		return nil, nil, fmt.Errorf("unexpected ForkchoiceUpdate response status: %s", fcRes.PayloadStatus.Status)
	}
	if fcRes.PayloadID == nil {
		return nil, nil, errors.New("empty payload ID")
	}

	// Step 2, get the payload
	payload, err := s.rpc.L2Engine.GetPayload(ctx, fcRes.PayloadID)
	if err != nil {
		return nil, err, nil
	}

	// Step 3, execute the payload
	execStatus, err := s.rpc.L2Engine.NewPayload(ctx, payload)
	if err != nil {
		return nil, err, nil
	}
	if execStatus.Status != beacon.VALID {
		return nil, nil, fmt.Errorf("unexpected NewPayload response status: %s", execStatus.Status)
	}

	return payload, nil, nil
}

// getInvalidateBlockTxOpts signs the transaction with a the
// throwaway blocks builder private key.
func (s *Syncer) getInvalidateBlockTxOpts(ctx context.Context, height *big.Int) (*bind.TransactOpts, error) {
	opts, err := bind.NewKeyedTransactorWithChainID(
		s.throwawayBlocksBuilderPrivKey,
		s.rpc.L2ChainID,
	)
	if err != nil {
		return nil, err
	}

	nonce, err := s.rpc.L2AccountNonce(
		ctx,
		crypto.PubkeyToAddress(s.throwawayBlocksBuilderPrivKey.PublicKey),
		height,
	)
	if err != nil {
		return nil, err
	}

	opts.GasPrice = common.Big0
	opts.Nonce = new(big.Int).SetUint64(nonce)
	opts.NoSend = true

	return opts, nil
}
