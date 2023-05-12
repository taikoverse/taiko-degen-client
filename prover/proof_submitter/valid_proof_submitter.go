package submitter

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/taikoxyz/taiko-client/bindings"
	"github.com/taikoxyz/taiko-client/bindings/encoding"
	"github.com/taikoxyz/taiko-client/metrics"
	"github.com/taikoxyz/taiko-client/pkg/rpc"
	anchorTxValidator "github.com/taikoxyz/taiko-client/prover/anchor_tx_validator"
	proofProducer "github.com/taikoxyz/taiko-client/prover/proof_producer"
)

var _ ProofSubmitter = (*ValidProofSubmitter)(nil)

// ValidProofSubmitter is responsible requesting zk proofs for the given valid L2
// blocks, and submitting the generated proofs to the TaikoL1 smart contract.
type ValidProofSubmitter struct {
	rpc               *rpc.Client
	proofProducer     proofProducer.ProofProducer
	resultCh          chan *proofProducer.ProofWithHeader
	anchorTxValidator *anchorTxValidator.AnchorTxValidator
	proverPrivKey     *ecdsa.PrivateKey
	proverAddress     common.Address
	taikoL2Address    common.Address
	l1SignalService   common.Address
	l2SignalService   common.Address
	mutex             *sync.Mutex
	isOracleProver    bool
	isSystemProver    bool
	graffiti          [32]byte
}

// NewValidProofSubmitter creates a new ValidProofSubmitter instance.
func NewValidProofSubmitter(
	rpc *rpc.Client,
	proofProducer proofProducer.ProofProducer,
	resultCh chan *proofProducer.ProofWithHeader,
	taikoL2Address common.Address,
	proverPrivKey *ecdsa.PrivateKey,
	mutex *sync.Mutex,
	isOracleProver bool,
	isSystemProver bool,
	graffiti string,
) (*ValidProofSubmitter, error) {
	anchorValidator, err := anchorTxValidator.New(taikoL2Address, rpc.L2ChainID, rpc)
	if err != nil {
		return nil, err
	}

	var signalServiceNameBytes [32]byte
	copy(signalServiceNameBytes[:], []byte("signal_service"))

	l1SignalService, err := rpc.TaikoL1.Resolve0(nil, signalServiceNameBytes, false)
	if err != nil {
		return nil, err
	}

	l2SignalService, err := rpc.TaikoL2.Resolve0(nil, signalServiceNameBytes, false)
	if err != nil {
		return nil, err
	}

	var graffitiBytes [32]byte
	copy(graffitiBytes[:], []byte(graffiti))

	return &ValidProofSubmitter{
		rpc:               rpc,
		proofProducer:     proofProducer,
		resultCh:          resultCh,
		anchorTxValidator: anchorValidator,
		proverPrivKey:     proverPrivKey,
		proverAddress:     crypto.PubkeyToAddress(proverPrivKey.PublicKey),
		l1SignalService:   l1SignalService,
		l2SignalService:   l2SignalService,
		taikoL2Address:    taikoL2Address,
		mutex:             mutex,
		isOracleProver:    isOracleProver,
		isSystemProver:    isSystemProver,
		graffiti:          graffitiBytes,
	}, nil
}

// RequestProof implements the ProofSubmitter interface.
func (s *ValidProofSubmitter) RequestProof(ctx context.Context, event *bindings.TaikoL1ClientBlockProposed) error {
	l1Origin, err := s.rpc.WaitL1Origin(ctx, event.Id)
	if err != nil {
		return fmt.Errorf("failed to fetch l1Origin, blockID: %d, err: %w", event.Id, err)
	}

	// Get the header of the block to prove from L2 execution engine.
	block, err := s.rpc.L2.BlockByHash(ctx, l1Origin.L2BlockHash)
	if err != nil {
		return err
	}

	parent, err := s.rpc.L2.BlockByHash(ctx, block.ParentHash())
	if err != nil {
		return err
	}

	blockInfo, err := s.rpc.TaikoL1.GetBlock(nil, event.Id)
	if err != nil {
		return err
	}

	if block.Transactions().Len() == 0 {
		return errors.New("no transaction in block")
	}

	signalRoot, err := s.rpc.GetStorageRoot(ctx, s.rpc.L2GethClient, s.l2SignalService, block.Number())
	if err != nil {
		return fmt.Errorf("error getting storageroot: %w", err)
	}

	// Request proof.
	opts := &proofProducer.ProofRequestOptions{
		Height:             block.Number(),
		ProverAddress:      s.proverAddress,
		ProposeBlockTxHash: event.Raw.TxHash,
		L1SignalService:    s.l1SignalService,
		L2SignalService:    s.l2SignalService,
		TaikoL2:            s.taikoL2Address,
		MetaHash:           blockInfo.MetaHash,
		BlockHash:          block.Hash(),
		ParentHash:         block.ParentHash(),
		SignalRoot:         signalRoot,
		Graffiti:           common.Bytes2Hex(s.graffiti[:]),
		GasUsed:            block.GasUsed(),
		ParentGasUsed:      parent.GasUsed(),
	}

	if err := s.proofProducer.RequestProof(ctx, opts, event.Id, &event.Meta, block.Header(), s.resultCh); err != nil {
		return err
	}

	metrics.ProverQueuedProofCounter.Inc(1)
	metrics.ProverQueuedValidProofCounter.Inc(1)

	return nil
}

// SubmitProof implements the ProofSubmitter interface.
func (s *ValidProofSubmitter) SubmitProof(
	ctx context.Context,
	proofWithHeader *proofProducer.ProofWithHeader,
) (err error) {
	log.Info(
		"New valid block proof",
		"blockID", proofWithHeader.BlockID,
		"beneficiary", proofWithHeader.Meta.Beneficiary,
		"hash", proofWithHeader.Header.Hash(),
		"proof", common.Bytes2Hex(proofWithHeader.ZkProof),
		"graffiti", common.Bytes2Hex(s.graffiti[:]),
	)
	var (
		blockID = proofWithHeader.BlockID
		header  = proofWithHeader.Header
		zkProof = proofWithHeader.ZkProof
	)

	metrics.ProverReceivedProofCounter.Inc(1)
	metrics.ProverReceivedValidProofCounter.Inc(1)

	// Get the corresponding L2 block.
	block, err := s.rpc.L2.BlockByHash(ctx, header.Hash())
	if err != nil {
		return fmt.Errorf("failed to get L2 block with given hash %s: %w", header.Hash(), err)
	}

	log.Debug(
		"Get the L2 block to prove",
		"blockID", blockID,
		"hash", block.Hash(),
		"root", header.Root.String(),
		"transactions", len(block.Transactions()),
	)

	if block.Transactions().Len() == 0 {
		return fmt.Errorf("invalid block without anchor transaction, blockID %s", blockID)
	}

	// Validate TaikoL2.anchor transaction inside the L2 block.
	anchorTx := block.Transactions()[0]
	if err := s.anchorTxValidator.ValidateAnchorTx(ctx, anchorTx); err != nil {
		return fmt.Errorf("invalid anchor transaction: %w", err)
	}

	// Get and validate this anchor transaction's receipt.
	_, err = s.anchorTxValidator.GetAndValidateAnchorTxReceipt(ctx, anchorTx)
	if err != nil {
		return fmt.Errorf("failed to fetch anchor transaction receipt: %w", err)
	}

	signalRoot, err := s.anchorTxValidator.GetAnchoredSignalRoot(ctx, anchorTx)
	if err != nil {
		return err
	}

	parent, err := s.rpc.L2.BlockByHash(ctx, block.ParentHash())
	if err != nil {
		return err
	}

	blockInfo, err := s.rpc.TaikoL1.GetBlock(nil, blockID)
	if err != nil {
		return err
	}

	evidence := &encoding.TaikoL1Evidence{
		MetaHash:      blockInfo.MetaHash,
		ParentHash:    block.ParentHash(),
		BlockHash:     block.Hash(),
		SignalRoot:    signalRoot,
		Graffiti:      s.graffiti,
		ParentGasUsed: uint32(parent.GasUsed()),
		GasUsed:       uint32(block.GasUsed()),
		Proof:         zkProof,
	}

	var circuitsIdx uint16
	var prover common.Address

	if s.isOracleProver || s.isSystemProver {
		if s.isOracleProver {
			prover = common.HexToAddress("0x0000000000000000000000000000000000000000")
		} else {
			prover = common.HexToAddress("0x0000000000000000000000000000000000000001")
		}
		circuitsIdx = uint16(int(zkProof[64]))
		evidence.Proof = zkProof[0:64]
	} else {
		prover = s.proverAddress

		circuitsIdx, err = proofProducer.DegreeToCircuitsIdx(proofWithHeader.Degree)
		if err != nil {
			return err
		}
	}
	evidence.Prover = prover
	evidence.VerifierId = circuitsIdx

	input, err := encoding.EncodeProveBlockInput(evidence)
	if err != nil {
		return fmt.Errorf("failed to encode TaikoL1.proveBlock inputs: %w", err)
	}

	// Send the TaikoL1.proveBlock transaction.
	txOpts, err := getProveBlocksTxOpts(ctx, s.rpc.L1, s.rpc.L1ChainID, s.proverPrivKey)
	if err != nil {
		return err
	}

	sendTx := func() (*types.Transaction, error) {
		s.mutex.Lock()
		defer s.mutex.Unlock()

		return s.rpc.TaikoL1.ProveBlock(txOpts, blockID, input)
	}

	if err := sendTxWithBackoff(ctx, s.rpc, blockID, sendTx); err != nil {
		if errors.Is(err, errUnretryable) {
			return nil
		}

		return err
	}

	log.Info(
		"✅ Valid block proved",
		"blockID", proofWithHeader.BlockID,
		"hash", block.Hash(), "height", block.Number(),
		"transactions", block.Transactions().Len(),
	)

	metrics.ProverSentProofCounter.Inc(1)
	metrics.ProverSentValidProofCounter.Inc(1)
	metrics.ProverLatestProvenBlockIDGauge.Update(proofWithHeader.BlockID.Int64())

	return nil
}

// CancelProof cancels an existing proof generation.
// Right now, it is just a stub that does nothing, because it is not possible to cnacel the proof
// with the current zkevm software.
func (s *ValidProofSubmitter) CancelProof(ctx context.Context, blockID *big.Int) error {
	return s.proofProducer.Cancel(ctx, blockID)
}
