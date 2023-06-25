package calldata

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"math/rand"
	"time"
	// "encoding/json"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	// "github.com/taikoxyz/taiko-client/bindings"
	"github.com/taikoxyz/taiko-client/bindings/encoding"
	"github.com/taikoxyz/taiko-client/metrics"
	eventIterator "github.com/taikoxyz/taiko-client/pkg/chain_iterator/event_iterator"
	"github.com/taikoxyz/taiko-client/pkg/rpc"
	txListValidator "github.com/taikoxyz/taiko-client/pkg/tx_list_validator"
	anchorTxConstructor "github.com/taikoxyz/taiko-client/proposer/anchor_tx_constructor"
	"github.com/taikoxyz/taiko-client/proposer/chain_syncer/beaconsync"
	// "github.com/taikoxyz/taiko-client/proposer/state"
)

// Syncer responsible for letting the L2 execution engine catching up with protocol's latest
// pending block through deriving L1 calldata.
type Syncer struct {
	ctx               context.Context
	rpc               *rpc.Client
	// state             *state.State
	progressTracker   *beaconsync.SyncProgressTracker          // Sync progress tracker
	anchorConstructor *anchorTxConstructor.AnchorTxConstructor // TaikoL2.anchor transactions constructor
	txListValidator   *txListValidator.TxListValidator         // Transactions list validator
	// Used by BlockInserter
	lastInsertedBlockID *big.Int
	reorgDetectedFlag   bool
}

// NewSyncer creates a new syncer instance.
func NewSyncer(
	ctx context.Context,
	rpc *rpc.Client,
	// state *state.State,
	progressTracker *beaconsync.SyncProgressTracker,
	signalServiceAddress common.Address,
) (*Syncer, error) {
	configs, err := rpc.TaikoL1.GetConfig(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get protocol configs: %w", err)
	}

	constructor, err := anchorTxConstructor.New(rpc, signalServiceAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize anchor constructor: %w", err)
	}

	return &Syncer{
		ctx:               ctx,
		rpc:               rpc,
		// state:             state,
		progressTracker:   progressTracker,
		anchorConstructor: constructor,
		txListValidator: txListValidator.NewTxListValidator(
			configs.BlockMaxGasLimit,
			configs.MaxTransactionsPerBlock,
			configs.MaxBytesPerTxList,
			rpc.L2ChainID,
		),
	}, nil
}

// ProcessL1Blocks fetches all `TaikoL1.BlockProposed` events between given
// L1 block heights, and then tries inserting them into L2 execution engine's block chain.
func (s *Syncer) ProcessL1Blocks(ctx context.Context, l1End *types.Header) error {
	firstTry := true
	for firstTry || s.reorgDetectedFlag {
		s.reorgDetectedFlag = false
		firstTry = false

		iter, err := eventIterator.NewBlockProposedIterator(ctx, &eventIterator.BlockProposedIteratorConfig{
			Client:               s.rpc.L1,
			TaikoL1:              s.rpc.TaikoL1,
			StartHeight:          nil,
			EndHeight:            l1End.Number,
			FilterQuery:          nil,
			OnBlockProposedEvent: nil,
		})
		if err != nil {
			return err
		}

		if err := iter.Iter(); err != nil {
			return err
		}
	}

	// s.state.SetL1Current(l1End)
	// metrics.DriverL1CurrentHeightGauge.Update(s.state.GetL1Current().Number.Int64())

	return nil
}

// OnBlockProposed is a `BlockProposed` event callback which responsible for
// inserting the proposed block one by one to the L2 execution engine.
func (s *Syncer) OnBlockProposed(
	ctx context.Context,
	// event *bindings.TaikoL1ClientBlockProposed,
	// endIter eventIterator.EndBlockProposedEventIterFunc,
	meta *encoding.TaikoL1BlockMetadataInput,
	txListBytes []byte,
) error {
	// if event.Id.Cmp(common.Big0) == 0 {
	// 	return nil
	// }

	// if !s.progressTracker.Triggered() {
	// 	// Check whteher we need to reorg the L2 chain at first.
	// 	reorged, l1CurrentToReset, lastInsertedBlockIDToReset, err := s.rpc.CheckL1Reorg(
	// 		ctx,
	// 		new(big.Int).Sub(event.Id, common.Big1),
	// 	)
	// 	if err != nil {
	// 		return fmt.Errorf("failed to check whether L1 chain has been reorged: %w", err)
	// 	}

	// 	if reorged {
	// 		log.Info(
	// 			"Reset L1Current cursor due to L1 reorg",
	// 			"l1CurrentHeightOld", s.state.GetL1Current().Number,
	// 			"l1CurrentHashOld", s.state.GetL1Current().Hash(),
	// 			"l1CurrentHeightNew", l1CurrentToReset.Number,
	// 			"l1CurrentHashNew", l1CurrentToReset.Hash(),
	// 			"lastInsertedBlockIDOld", s.lastInsertedBlockID,
	// 			"lastInsertedBlockIDNew", lastInsertedBlockIDToReset,
	// 		)
	// 		s.state.SetL1Current(l1CurrentToReset)
	// 		s.lastInsertedBlockID = lastInsertedBlockIDToReset
	// 		s.reorgDetectedFlag = true
	// 		endIter()

	// 		return nil
	// 	}
	// }

	// // Ignore those already inserted blocks.
	// if s.lastInsertedBlockID != nil && event.Id.Cmp(s.lastInsertedBlockID) <= 0 {
	// 	return nil
	// }

	// log.Info(
	// 	"New BlockProposed event",
	// 	"L1Height", event.Raw.BlockNumber,
	// 	"L1Hash", event.Raw.BlockHash,
	// 	"BlockID", event.Id,
	// 	"BlockFee", event.BlockFee,
	// 	"Removed", event.Raw.Removed,
	// )

	// Fetch the L2 parent block.
	var (
		parent *types.Header
		err    error
	)
	// if s.progressTracker.Triggered() {
	// 	// Already synced through beacon sync, just skip this event.
	// 	if event.Id.Cmp(s.progressTracker.LastSyncedVerifiedBlockID()) <= 0 {
	// 		return nil
	// 	}

	// 	parent, err = s.rpc.L2.HeaderByHash(ctx, s.progressTracker.LastSyncedVerifiedBlockHash())
	// } else {
	parentId, err := s.rpc.L2.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch L2 parent block id: %w", err)
	}

	parent, err = s.rpc.L2ParentByBlockId(ctx, new(big.Int).SetUint64(parentId + 1))
	if err != nil {
		return fmt.Errorf("failed to fetch L2 parent block: %w", err)
	}

	log.Info("Parent block", "height", parent.Number, "hash", parent.Hash())

	// tx, err := s.rpc.L1.TransactionInBlock(
	// 	ctx,
	// 	event.Raw.BlockHash,
	// 	event.Raw.TxIndex,
	// )
	// if err != nil {
	// 	return fmt.Errorf("failed to fetch original TaikoL1.proposeBlock transaction: %w", err)
	// }

	// // Check whether the transactions list is valid.
	// txListBytes, hint, invalidTxIndex, err := s.txListValidator.ValidateTxList(event.Id, tx.Data())
	// if err != nil {
	// 	return fmt.Errorf("failed to validate transactions list: %w", err)
	// }
	
	currentId := parentId + 1
	log.Info(
		"Validate transactions list",
		"blockID", currentId,
		// "hint", hint,
		// "invalidTxIndex", invalidTxIndex,
	)

	l1Origin := &rawdb.L1Origin{
		BlockID:       new(big.Int).SetUint64(currentId),
		L2BlockHash:   common.Hash{}, // Will be set by taiko-geth.
		L1BlockHeight: new(big.Int).SetUint64(0), // todo: temporarily set to 0
		L1BlockHash:   common.BigToHash(new(big.Int).SetUint64(0)), // todo: temporarily set to 0
	}


	// if event.Meta.Timestamp > uint64(time.Now().Unix()) {
	// 	log.Warn("Future L2 block, waiting", "L2BlockTimestamp", event.Meta.Timestamp, "now", time.Now().Unix())
	// 	time.Sleep(time.Until(time.Unix(int64(event.Meta.Timestamp), 0)))
	// }

	// // If the transactions list is invalid, we simply insert an empty L2 block.
	// if hint != txListValidator.HintOK {
	// 	log.Info("Invalid transactions list, insert an empty L2 block instead", "blockID", event.Id)
	// 	txListBytes = []byte{}
	// }

	payloadData, err := s.insertNewHead(
		ctx,
		// event,
		parent,
		parent.Number,
		txListBytes,
		l1Origin,
		currentId,
		meta,
	)

	if err != nil {
		return fmt.Errorf("failed to insert new head to L2 execution engine: %w", err)
	}

	log.Debug("Payload data", "hash", payloadData.BlockHash, "txs", len(payloadData.Transactions))

	log.Info(
		"🔗 New L2 block inserted",
		"blockID", currentId,
		"height", payloadData.Number,	
		"hash", payloadData.BlockHash,
		// "latestVerifiedBlockHeight", s.state.GetLatestVerifiedBlock().Height,
		// "latestVerifiedBlockHash", s.state.GetLatestVerifiedBlock().Hash,
		"transactions", len(payloadData.Transactions),
		"baseFee", payloadData.BaseFeePerGas,
		"withdrawals", len(payloadData.Withdrawals),
	)

	metrics.DriverL1CurrentHeightGauge.Update(int64(0))
	s.lastInsertedBlockID = new(big.Int).SetUint64(currentId)

	if s.progressTracker.Triggered() {
		s.progressTracker.ClearMeta()
	}

	return nil
}

// insertNewHead tries to insert a new head block to the L2 execution engine's local
// block chain through Engine APIs.
func (s *Syncer) insertNewHead(
	ctx context.Context,
	// event *bindings.TaikoL1ClientBlockProposed,
	parent *types.Header,
	headBlockID *big.Int,
	txListBytes []byte,
	l1Origin *rawdb.L1Origin,
	currentId uint64,
	meta *encoding.TaikoL1BlockMetadataInput,
) (*engine.ExecutableData, error) {
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
			log.Error("Invalid txList bytes", "blockID", currentId)
			return nil, err
		}
	}

	metaTimestamp := uint64(time.Now().Unix())
	parentTimestamp, err := s.rpc.TaikoL2.ParentTimestamp(&bind.CallOpts{BlockNumber: parent.Number})
	if err != nil {
		return nil, err
	}

	// Get L2 baseFee
	baseFee, err := s.rpc.TaikoL2.GetBasefee(
		&bind.CallOpts{BlockNumber: parent.Number},
		uint32(metaTimestamp-parentTimestamp),
		uint64(meta.GasLimit+uint32(s.anchorConstructor.GasLimit())),
		parent.GasUsed,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get L2 baseFee: %w", encoding.TryParsingCustomError(err))
	}

	log.Debug(
		"GetBasefee",
		"baseFee", baseFee,
		"timeSinceParent", uint32(metaTimestamp-parentTimestamp),
		"gasLimit", uint64(meta.GasLimit+uint32(s.anchorConstructor.GasLimit())),
		"parentGasUsed", parent.GasUsed,
	)

	// Get withdrawals
	withdrawals := make(types.Withdrawals, 0)
	// for i, d := range event.Meta.DepositsProcessed {
	// 	withdrawals[i] = &types.Withdrawal{Address: d.Recipient, Amount: d.Amount.Uint64(), Index: d.Id}
	// }

	// Assemble a TaikoL2.anchor transaction
	anchorTx, err := s.anchorConstructor.AssembleAnchorTx(
		ctx,
		// new(big.Int).SetUint64(event.Meta.L1Height),
		// event.Meta.L1Hash,
		new(big.Int).SetUint64(0),
		common.BigToHash(new(big.Int).SetUint64(0)),
		new(big.Int).Add(parent.Number, common.Big1),
		baseFee,
		parent.GasUsed,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create TaikoL2.anchor transaction: %w", err)
	}

	txList = append([]*types.Transaction{anchorTx}, txList...)

	if txListBytes, err = rlp.EncodeToBytes(txList); err != nil {
		log.Error("Encode txList error", "blockID", currentId, "error", err)
		return nil, err
	}

	payload, err := s.createExecutionPayloads(
		ctx,
		// event,
		parent.Hash(),
		l1Origin,
		headBlockID,
		txListBytes,
		baseFee,
		withdrawals,
		int64(metaTimestamp),
		meta,
	)

	if err != nil {
		return nil, fmt.Errorf("failed to create execution payloads: %w", err)
	}

	fc := &engine.ForkchoiceStateV1{HeadBlockHash: parent.Hash()}

	// Update the fork choice
	fc.HeadBlockHash = payload.BlockHash
	fcRes, err := s.rpc.L2Engine.ForkchoiceUpdate(ctx, fc, nil)
	if err != nil {
		return nil, err
	}
	if fcRes.PayloadStatus.Status != engine.VALID {
		return nil, fmt.Errorf("unexpected ForkchoiceUpdate response status: %s", fcRes.PayloadStatus.Status)
	}

	return payload, nil
}

// createExecutionPayloads creates a new execution payloads through
// Engine APIs.
func (s *Syncer) createExecutionPayloads(
	ctx context.Context,
	// event *bindings.TaikoL1ClientBlockProposed,
	parentHash common.Hash,
	l1Origin *rawdb.L1Origin,
	headBlockID *big.Int,
	txListBytes []byte,
	baseFeee *big.Int,
	withdrawals types.Withdrawals,
	metaTimestamp int64,
	meta *encoding.TaikoL1BlockMetadataInput,
) (payloadData *engine.ExecutableData, err error) {
	fc := &engine.ForkchoiceStateV1{HeadBlockHash: parentHash}
	random := common.BigToHash(new(big.Int).SetUint64(rand.Uint64()))
	attributes := &engine.PayloadAttributes{
		Timestamp:             uint64(metaTimestamp),
		Random:                random,
		SuggestedFeeRecipient: meta.Beneficiary,
		Withdrawals:           withdrawals,
		BlockMetadata: &engine.BlockMetadata{
			HighestBlockID: headBlockID,
			Beneficiary:    meta.Beneficiary,
			GasLimit:       uint64(meta.GasLimit) + s.anchorConstructor.GasLimit(),
			Timestamp:      uint64(metaTimestamp),
			TxList:         txListBytes,
			MixHash:        random,
			ExtraData:      []byte{},
		},
		BaseFeePerGas: baseFeee,
		L1Origin:      l1Origin,
	}

	log.Debug("PayloadAttributes", "attributes", attributes, "meta", attributes.BlockMetadata)

	// Step 1, prepare a payload
	fcRes, err := s.rpc.L2Engine.ForkchoiceUpdate(ctx, fc, attributes)
	if err != nil {
		return nil, fmt.Errorf("failed to update fork choice: %w", err)
	}
	if fcRes.PayloadStatus.Status != engine.VALID {
		return nil, fmt.Errorf("unexpected ForkchoiceUpdate response status: %s", fcRes.PayloadStatus.Status)
	}
	if fcRes.PayloadID == nil {
		return nil, errors.New("empty payload ID")
	}

	// Step 2, get the payload
	payload, err := s.rpc.L2Engine.GetPayload(ctx, fcRes.PayloadID)
	if err != nil {
		return nil, fmt.Errorf("failed to get payload: %w", err)
	}

	log.Debug("Payload", "payload", payload)

	// Step 3, execute the payload
	execStatus, err := s.rpc.L2Engine.NewPayload(ctx, payload)
	if err != nil {
		return nil, fmt.Errorf("failed to create a new payload: %w", err)
	}
	if execStatus.Status != engine.VALID {
		return nil, fmt.Errorf("unexpected NewPayload response status: %s", execStatus.Status)
	}

	return payload, nil
}
