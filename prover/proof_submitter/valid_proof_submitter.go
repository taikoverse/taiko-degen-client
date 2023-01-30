package submitter

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
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
	reusltCh          chan *proofProducer.ProofWithHeader
	anchorTxValidator *anchorTxValidator.AnchorTxValidator
	proverPrivKey     *ecdsa.PrivateKey
	proverAddress     common.Address
	zkProofsPerBlock  uint64
	mutex             *sync.Mutex
}

// NewValidProofSubmitter creates a new ValidProofSubmitter instance.
func NewValidProofSubmitter(
	rpc *rpc.Client,
	proofProducer proofProducer.ProofProducer,
	reusltCh chan *proofProducer.ProofWithHeader,
	taikoL2Address common.Address,
	proverPrivKey *ecdsa.PrivateKey,
	zkProofsPerBlock uint64,
	mutex *sync.Mutex,
) *ValidProofSubmitter {
	return &ValidProofSubmitter{
		rpc:               rpc,
		proofProducer:     proofProducer,
		reusltCh:          reusltCh,
		anchorTxValidator: anchorTxValidator.New(taikoL2Address, rpc.L2ChainID, rpc),
		proverPrivKey:     proverPrivKey,
		proverAddress:     crypto.PubkeyToAddress(proverPrivKey.PublicKey),
		zkProofsPerBlock:  zkProofsPerBlock,
		mutex:             mutex,
	}
}

// RequestProof implements the ProofSubmitter interface.
func (s *ValidProofSubmitter) RequestProof(ctx context.Context, event *bindings.TaikoL1ClientBlockProposed) error {
	l1Origin, err := s.rpc.WaitL1Origin(ctx, event.Id)
	if err != nil {
		return fmt.Errorf("failed to fetch l1Origin, blockID: %d, err: %w", event.Id, err)
	}

	// This should not be reached, only check for safety.
	if l1Origin.Throwaway {
		log.Error("Get a block metadata with invalid transaction list", "l1Origin", l1Origin)
		return nil
	}

	// Get the header of the block to prove from L2 execution engine.
	header, err := s.rpc.L2.HeaderByHash(ctx, l1Origin.L2BlockHash)
	if err != nil {
		return err
	}

	// Request proof.
	opts := &proofProducer.ProofRequestOptions{
		Height: header.Number,
	}

	if err := s.proofProducer.RequestProof(opts, event.Id, &event.Meta, header, s.reusltCh); err != nil {
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
		"proof", proofWithHeader.ZkProof,
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
	anchorTxReceipt, err := s.anchorTxValidator.GetAndValidateAnchorTxReceipt(ctx, anchorTx)
	if err != nil {
		return fmt.Errorf("failed to fetch anchor transaction receipt: %w", err)
	}

	// Generate the merkel proof (whose root is block's txRoot) of this anchor transaction.
	txRoot, anchorTxProof, err := generateTrieProof(block.Transactions(), 0)
	if err != nil {
		return fmt.Errorf("failed to generate anchor transaction proof: %w", err)
	}

	// Generate the merkel proof (whose root is block's receiptRoot) of this anchor transaction's receipt.
	receipts, err := rpc.GetReceiptsByBlock(ctx, s.rpc.L2RawRPC, block)
	if err != nil {
		return fmt.Errorf("failed to fetch block receipts: %w", err)
	}
	receiptRoot, anchorReceiptProof, err := generateTrieProof(receipts, 0)
	if err != nil {
		return fmt.Errorf("failed to generate anchor receipt proof: %w", err)
	}

	// Double check the calculated roots.
	if txRoot != block.TxHash() || receiptRoot != block.ReceiptHash() {
		return fmt.Errorf(
			"txHash or receiptHash mismatch, txRoot: %s, header.TxHash: %s, receiptRoot: %s, header.ReceiptHash: %s",
			txRoot, header.TxHash, receiptRoot, header.ReceiptHash,
		)
	}

	// Assemble the TaikoL1.proveBlock transaction inputs.
	proofs := [][]byte{}
	for i := 0; i < int(s.zkProofsPerBlock); i++ {
		proofs = append(proofs, zkProof)
	}
	proofs = append(proofs, [][]byte{anchorTxProof, anchorReceiptProof}...)

	evidence := &encoding.TaikoL1Evidence{
		Meta:     *proofWithHeader.Meta,
		Header:   *encoding.FromGethHeader(header),
		Prover:   s.proverAddress,
		Proofs:   proofs,
		Circuits: []uint16{0},
	}

	input, err := encoding.EncodeProveBlockInput(evidence, anchorTx, anchorTxReceipt)
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

	return nil
}
