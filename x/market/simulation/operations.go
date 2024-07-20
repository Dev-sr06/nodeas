package simulation

import (
	"errors"
	"fmt"
	"math/rand"

	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/codec"
	simtestutil "github.com/cosmos/cosmos-sdk/testutil/sims"
	sdk "github.com/cosmos/cosmos-sdk/types"
	moduletestutil "github.com/cosmos/cosmos-sdk/types/module/testutil"
	simtypes "github.com/cosmos/cosmos-sdk/types/simulation"
	"github.com/cosmos/cosmos-sdk/x/simulation"

	"pkg.akt.dev/go/node/market/v1"
	types "pkg.akt.dev/go/node/market/v1beta5"

	appparams "pkg.akt.dev/akashd/app/params"
	testsim "pkg.akt.dev/akashd/testutil/sim"
	keepers "pkg.akt.dev/akashd/x/market/handler"
)

// Simulation operation weights constants
const (
	OpWeightMsgCreateBid  = "op_weight_msg_create_bid"  // nolint gosec
	OpWeightMsgCloseBid   = "op_weight_msg_close_bid"   // nolint gosec
	OpWeightMsgCloseLease = "op_weight_msg_close_lease" // nolint gosec
)

// WeightedOperations returns all the operations from the module with their respective weights
func WeightedOperations(
	appParams simtypes.AppParams, cdc codec.JSONCodec, ks keepers.Keepers) simulation.WeightedOperations {
	var (
		weightMsgCreateBid  int
		weightMsgCloseBid   int
		weightMsgCloseLease int
	)

	appParams.GetOrGenerate(
		cdc, OpWeightMsgCreateBid, &weightMsgCreateBid, nil, func(r *rand.Rand) {
			weightMsgCreateBid = appparams.DefaultWeightMsgCreateBid
		},
	)

	appParams.GetOrGenerate(
		cdc, OpWeightMsgCloseBid, &weightMsgCloseBid, nil, func(r *rand.Rand) {
			weightMsgCloseBid = appparams.DefaultWeightMsgCloseBid
		},
	)

	appParams.GetOrGenerate(
		cdc, OpWeightMsgCloseLease, &weightMsgCloseLease, nil, func(r *rand.Rand) {
			weightMsgCloseLease = appparams.DefaultWeightMsgCloseLease
		},
	)

	return simulation.WeightedOperations{
		simulation.NewWeightedOperation(
			weightMsgCreateBid,
			SimulateMsgCreateBid(ks),
		),
		simulation.NewWeightedOperation(
			weightMsgCloseBid,
			SimulateMsgCloseBid(ks),
		),
		simulation.NewWeightedOperation(
			weightMsgCloseLease,
			SimulateMsgCloseLease(ks),
		),
	}
}

// SimulateMsgCreateBid generates a MsgCreateBid with random values
func SimulateMsgCreateBid(ks keepers.Keepers) simtypes.Operation {
	return func(r *rand.Rand, app *baseapp.BaseApp, ctx sdk.Context, accounts []simtypes.Account,
		chainID string) (simtypes.OperationMsg, []simtypes.FutureOperation, error) {
		orders := getOrdersWithState(ctx, ks, types.OrderOpen)
		if len(orders) == 0 {
			return simtypes.NoOpMsg(types.ModuleName, (&types.MsgCreateBid{}).Type(), "no open orders found"), nil, nil
		}

		// Get random order
		order := orders[testsim.RandIdx(r, len(orders)-1)]

		providers := getProviders(ctx, ks)

		if len(providers) == 0 {
			return simtypes.NoOpMsg(types.ModuleName, (&types.MsgCreateBid{}).Type(), "no providers found"), nil, nil
		}

		// Get random deployment
		provider := providers[testsim.RandIdx(r, len(providers)-1)]

		ownerAddr, convertErr := sdk.AccAddressFromBech32(provider.Owner)
		if convertErr != nil {
			return simtypes.NoOpMsg(types.ModuleName, (&types.MsgCreateBid{}).Type(), "error while converting address"), nil, convertErr
		}

		simAccount, found := simtypes.FindAccount(accounts, ownerAddr)
		if !found {
			return simtypes.NoOpMsg(types.ModuleName, (&types.MsgCreateBid{}).Type(), "unable to find provider"),
				nil, fmt.Errorf("provider with %s not found", provider.Owner)
		}

		if provider.Owner == order.ID.Owner {
			return simtypes.NoOpMsg(types.ModuleName, (&types.MsgCreateBid{}).Type(), "provider and order owner cannot be same"),
				nil, nil
		}

		depositAmount := minDeposit
		account := ks.Account.GetAccount(ctx, simAccount.Address)
		spendable := ks.Bank.SpendableCoins(ctx, account.GetAddress())

		if spendable.AmountOf(depositAmount.Denom).LT(depositAmount.Amount.MulRaw(2)) {
			return simtypes.NoOpMsg(types.ModuleName, (&types.MsgCreateBid{}).Type(), "out of money"), nil, nil
		}
		spendable = spendable.Sub(depositAmount)

		fees, err := simtypes.RandomFees(r, ctx, spendable)
		if err != nil {
			return simtypes.NoOpMsg(types.ModuleName, (&types.MsgCreateBid{}).Type(), "unable to generate fees"), nil, err
		}

		msg := types.NewMsgCreateBid(order.ID, simAccount.Address, order.Price(), depositAmount, nil)

		txGen := moduletestutil.MakeTestEncodingConfig().TxConfig
		tx, err := simtestutil.GenSignedMockTx(
			r,
			txGen,
			[]sdk.Msg{msg},
			fees,
			simtestutil.DefaultGenTxGas,
			chainID,
			[]uint64{account.GetAccountNumber()},
			[]uint64{account.GetSequence()},
			simAccount.PrivKey,
		)
		if err != nil {
			return simtypes.NoOpMsg(types.ModuleName, msg.Type(), "unable to generate mock tx"), nil, err
		}

		_, _, err = app.SimDeliver(txGen.TxEncoder(), tx)
		switch {
		case err == nil:
			return simtypes.NewOperationMsg(msg, true, "", nil), nil, nil
		case errors.Is(err, types.ErrBidExists):
			return simtypes.NewOperationMsg(msg, false, "", nil), nil, nil
		default:
			return simtypes.NoOpMsg(types.ModuleName, msg.Type(), "unable to deliver mock tx"), nil, err
		}
	}
}

// SimulateMsgCloseBid generates a MsgCloseBid with random values
func SimulateMsgCloseBid(ks keepers.Keepers) simtypes.Operation {
	return func(r *rand.Rand, app *baseapp.BaseApp, ctx sdk.Context, accounts []simtypes.Account,
		chainID string) (simtypes.OperationMsg, []simtypes.FutureOperation, error) {
		var bids []types.Bid

		ks.Market.WithBids(ctx, func(bid types.Bid) bool {
			if bid.State == types.BidActive {
				lease, ok := ks.Market.GetLease(ctx, v1.LeaseID(bid.ID))
				if ok && lease.State == v1.LeaseActive {
					bids = append(bids, bid)
				}
			}

			return false
		})

		if len(bids) == 0 {
			return simtypes.NoOpMsg(types.ModuleName, (&types.MsgCloseBid{}).Type(), "no matched bids found"), nil, nil
		}

		// Get random bid
		bid := bids[testsim.RandIdx(r, len(bids)-1)]

		providerAddr, convertErr := sdk.AccAddressFromBech32(bid.ID.Provider)
		if convertErr != nil {
			return simtypes.NoOpMsg(types.ModuleName, (&types.MsgCloseBid{}).Type(), "error while converting address"), nil, convertErr
		}

		simAccount, found := simtypes.FindAccount(accounts, providerAddr)
		if !found {
			return simtypes.NoOpMsg(types.ModuleName, (&types.MsgCloseBid{}).Type(), "unable to find bid with provider"),
				nil, fmt.Errorf("bid with %s not found", bid.ID.Provider)
		}

		account := ks.Account.GetAccount(ctx, simAccount.Address)
		spendable := ks.Bank.SpendableCoins(ctx, account.GetAddress())

		fees, err := simtypes.RandomFees(r, ctx, spendable)
		if err != nil {
			return simtypes.NoOpMsg(types.ModuleName, (&types.MsgCloseBid{}).Type(), "unable to generate fees"), nil, err
		}

		msg := types.NewMsgCloseBid(bid.ID)

		txGen := moduletestutil.MakeTestEncodingConfig().TxConfig
		tx, err := simtestutil.GenSignedMockTx(
			r,
			txGen,
			[]sdk.Msg{msg},
			fees,
			simtestutil.DefaultGenTxGas,
			chainID,
			[]uint64{account.GetAccountNumber()},
			[]uint64{account.GetSequence()},
			simAccount.PrivKey,
		)
		if err != nil {
			return simtypes.NoOpMsg(types.ModuleName, msg.Type(), "unable to generate mock tx"), nil, err
		}

		_, _, err = app.SimDeliver(txGen.TxEncoder(), tx)
		if err != nil {
			return simtypes.NoOpMsg(types.ModuleName, msg.Type(), "unable to deliver tx"), nil, err
		}

		return simtypes.NewOperationMsg(msg, true, "", nil), nil, nil
	}
}

// SimulateMsgCloseLease generates a MsgCloseLease with random values
func SimulateMsgCloseLease(ks keepers.Keepers) simtypes.Operation {
	return func(r *rand.Rand, app *baseapp.BaseApp, ctx sdk.Context, accounts []simtypes.Account,
		chainID string) (simtypes.OperationMsg, []simtypes.FutureOperation, error) {
		// orders := getOrdersWithState(ctx, ks, v1.OrderActive)
		// if len(orders) == 0 {
		// 	return simtypes.NoOpMsg(types.ModuleName, (&types.MsgCloseLease{}).Type(), "no orders with state matched found"), nil, nil
		// }
		//
		// // Get random order
		// order := orders[testsim.RandIdx(r, len(orders) - 1)]
		//
		// ownerAddr, convertErr := sdk.AccAddressFromBech32(order.ID.Owner)
		// if convertErr != nil {
		// 	return simtypes.NoOpMsg(types.ModuleName, (&types.MsgCloseLease{}).Type(), "error while converting address"), nil, convertErr
		// }
		//
		// simAccount, found := simtypes.FindAccount(accounts, ownerAddr)
		// if !found {
		// 	return simtypes.NoOpMsg(types.ModuleName, (&types.MsgCloseLease{}).Type(), "unable to find order"),
		// 		nil, fmt.Errorf("order with %s not found", order.ID.Owner)
		// }
		//
		// account := ks.Account.GetAccount(ctx, simAccount.Address)
		// spendable := ks.Bank.SpendableCoins(ctx, account.GetAddress())
		//
		// fees, err := simtypes.RandomFees(r, ctx, spendable)
		// if err != nil {
		// 	return simtypes.NoOpMsg(types.ModuleName, (&types.MsgCloseLease{}).Type(), "unable to generate fees"), nil, err
		// }
		//
		// msg := types.NewMsgCloseLease(order.ID)
		//
		// txGen := moduletestutil.MakeTestEncodingConfig().TxConfig
		// tx, err := simtestutil.GenSignedMockTx(
		// 	r,
		// 	txGen,
		// 	[]sdk.Msg{msg},
		// 	fees,
		// 	simtestutil.DefaultGenTxGas,
		// 	chainID,
		// 	[]uint64{account.GetAccountNumber()},
		// 	[]uint64{account.GetSequence()},
		// 	simAccount.PrivKey,
		// )
		// if err != nil {
		// 	return simtypes.NoOpMsg(types.ModuleName, msg.Type(), "unable to generate mock tx"), nil, err
		// }
		//
		// _, _, err = app.SimDeliver(txGen.TxEncoder(), tx)
		// if err != nil {
		// 	return simtypes.NoOpMsg(types.ModuleName, msg.Type(), "unable to deliver tx"), nil, err
		// }

		return simtypes.NoOpMsg(types.ModuleName, (&types.MsgCloseLease{}).Type(), "skipping"), nil, nil
	}
}
