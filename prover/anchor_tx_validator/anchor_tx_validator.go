package anchorTxValidator

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/taikoxyz/taiko-client/bindings"
	"github.com/taikoxyz/taiko-client/bindings/encoding"
	"github.com/taikoxyz/taiko-client/pkg/rpc"
)

// AnchorTxValidator is responsible for validating the anchor transaction (TaikoL2.anchor) in
// each L2 block, which is always the first transaction.
type AnchorTxValidator struct {
	taikoL2Address common.Address
	chainID        *big.Int
	rpc            *rpc.Client
}

// New creates a new AnchorTxValidator instance.
func New(taikoL2Address common.Address, chainID *big.Int, rpc *rpc.Client) *AnchorTxValidator {
	return &AnchorTxValidator{taikoL2Address, chainID, rpc}
}

// validateAnchorTx checks whether the given transaction is a valid `TaikoL2.anchor` transaction.
func (v *AnchorTxValidator) ValidateAnchorTx(ctx context.Context, tx *types.Transaction) error {
	if tx.To() == nil || *tx.To() != v.taikoL2Address {
		return fmt.Errorf("invalid TaikoL2.anchor transaction to: %s, want: %s", tx.To(), v.taikoL2Address)
	}

	sender, err := types.LatestSignerForChainID(v.chainID).Sender(tx)
	if err != nil {
		return fmt.Errorf("failed to get TaikoL2.anchor transaction sender: %w", err)
	}

	if sender != bindings.GoldenTouchAddress {
		return fmt.Errorf("invalid TaikoL2.anchor transaction sender: %s", sender)
	}

	method, err := encoding.TaikoL2ABI.MethodById(tx.Data())
	if err != nil || method.Name != "anchor" {
		return fmt.Errorf("invalid TaikoL2.anchor transaction selector, err: %w", err)
	}

	return nil
}

// GetAndValidateAnchorTxReceipt gets and validates the `TaikoL2.anchor` transaction's receipt.
func (v *AnchorTxValidator) GetAndValidateAnchorTxReceipt(
	ctx context.Context,
	tx *types.Transaction,
) (*types.Receipt, error) {
	receipt, err := v.rpc.L2.TransactionReceipt(ctx, tx.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get TaikoL2.anchor transaction receipt, err: %w", err)
	}

	if receipt.Status != types.ReceiptStatusSuccessful {
		return nil, fmt.Errorf("invalid TaikoL2.anchor transaction receipt status: %d", receipt.Status)
	}

	if len(receipt.Logs) == 0 {
		return nil, fmt.Errorf("no event found in TaikoL2.anchor transaction receipt")
	}

	return receipt, nil
}
