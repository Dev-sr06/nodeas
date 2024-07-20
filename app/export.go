package app

import (
	"encoding/json"
	"log"
	"math/rand"

	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/spf13/viper"

	dbm "github.com/cometbft/cometbft-db"
	abci "github.com/cometbft/cometbft/abci/types"
	logger "github.com/cometbft/cometbft/libs/log"
	cmproto "github.com/cometbft/cometbft/proto/tendermint/types"

	servertypes "github.com/cosmos/cosmos-sdk/server/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/simulation"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	"github.com/cosmos/cosmos-sdk/x/staking"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
)

// ExportAppStateAndValidators exports the state of the application for a genesis
// file.
func (app *AkashApp) ExportAppStateAndValidators(
	forZeroHeight bool,
	jailAllowedAddrs []string,
	modulesToExport []string,
) (servertypes.ExportedApp, error) {
	// as if they could withdraw from the start of the next block
	ctx := app.NewContext(true, cmproto.Header{Height: app.LastBlockHeight()})

	// We export at last height + 1, because that's the height at which
	// Tendermint will start InitChain.
	height := app.LastBlockHeight() + 1

	if forZeroHeight {
		height = 0
		app.prepForZeroHeightGenesis(ctx, jailAllowedAddrs)
	}

	genState := app.MM.ExportGenesisForModules(ctx, app.cdc, modulesToExport)
	appState, err := json.MarshalIndent(genState, "", "  ")
	if err != nil {
		return servertypes.ExportedApp{}, err
	}

	validators, err := staking.WriteValidators(ctx, app.Keepers.Cosmos.Staking)
	if err != nil {
		return servertypes.ExportedApp{}, err
	}

	return servertypes.ExportedApp{
		AppState:        appState,
		Validators:      validators,
		Height:          height,
		ConsensusParams: app.BaseApp.GetConsensusParams(ctx),
	}, nil
}

// prepForZeroHeightGenesis prepare for fresh start at zero height
// NOTE zero height genesis is a temporary feature which will be deprecated
//
//	in favour of export at a block height
func (app *AkashApp) prepForZeroHeightGenesis(ctx sdk.Context, jailAllowedAddrs []string) {
	applyAllowedAddrs := false

	// Check if there is a allowed address list
	if len(jailAllowedAddrs) > 0 {
		applyAllowedAddrs = true
	}

	allowedAddrsMap := make(map[string]bool)

	for _, addr := range jailAllowedAddrs {
		_, err := sdk.ValAddressFromBech32(addr)
		if err != nil {
			log.Fatal(err)
		}
		allowedAddrsMap[addr] = true
	}

	/* Just to be safe, assert the invariants on current state. */
	app.Keepers.Cosmos.Crisis.AssertInvariants(ctx)

	/* Handle fee distribution state. */

	// withdraw all validator commission
	app.Keepers.Cosmos.Staking.IterateValidators(ctx, func(_ int64, val stakingtypes.ValidatorI) (stop bool) {
		_, _ = app.Keepers.Cosmos.Distr.WithdrawValidatorCommission(ctx, val.GetOperator())
		return false
	})

	// withdraw all delegator rewards
	dels := app.Keepers.Cosmos.Staking.GetAllDelegations(ctx)
	for _, delegation := range dels {
		valAddr, err := sdk.ValAddressFromBech32(delegation.ValidatorAddress)
		if err != nil {
			panic(err)
		}

		delAddr, err := sdk.AccAddressFromBech32(delegation.DelegatorAddress)
		if err != nil {
			panic(err)
		}
		_, _ = app.Keepers.Cosmos.Distr.WithdrawDelegationRewards(ctx, delAddr, valAddr)
	}

	// clear validator slash events
	app.Keepers.Cosmos.Distr.DeleteAllValidatorSlashEvents(ctx)

	// clear validator historical rewards
	app.Keepers.Cosmos.Distr.DeleteAllValidatorHistoricalRewards(ctx)

	// set context height to zero
	height := ctx.BlockHeight()
	ctx = ctx.WithBlockHeight(0)

	// reinitialize all validators
	app.Keepers.Cosmos.Staking.IterateValidators(ctx, func(_ int64, val stakingtypes.ValidatorI) (stop bool) {
		// donate any unwithdrawn outstanding reward fraction tokens to the community pool
		scraps := app.Keepers.Cosmos.Distr.GetValidatorOutstandingRewardsCoins(ctx, val.GetOperator())
		feePool := app.Keepers.Cosmos.Distr.GetFeePool(ctx)
		feePool.CommunityPool = feePool.CommunityPool.Add(scraps...)
		app.Keepers.Cosmos.Distr.SetFeePool(ctx, feePool)

		_ = app.Keepers.Cosmos.Distr.Hooks().AfterValidatorCreated(ctx, val.GetOperator())

		return false
	})

	// reinitialize all delegations
	for _, del := range dels {
		valAddr, err := sdk.ValAddressFromBech32(del.ValidatorAddress)
		if err != nil {
			panic(err)
		}

		delAddr, err := sdk.AccAddressFromBech32(del.DelegatorAddress)
		if err != nil {
			panic(err)
		}
		_ = app.Keepers.Cosmos.Distr.Hooks().BeforeDelegationCreated(ctx, delAddr, valAddr)
		_ = app.Keepers.Cosmos.Distr.Hooks().AfterDelegationModified(ctx, delAddr, valAddr)
	}

	// reset context height
	ctx = ctx.WithBlockHeight(height)

	/* Handle staking state. */

	// iterate through redelegations, reset creation height
	app.Keepers.Cosmos.Staking.IterateRedelegations(ctx, func(_ int64, red stakingtypes.Redelegation) (stop bool) {
		for i := range red.Entries {
			red.Entries[i].CreationHeight = 0
		}
		app.Keepers.Cosmos.Staking.SetRedelegation(ctx, red)
		return false
	})

	// iterate through unbonding delegations, reset creation height
	app.Keepers.Cosmos.Staking.IterateUnbondingDelegations(ctx, func(_ int64, ubd stakingtypes.UnbondingDelegation) (stop bool) {
		for i := range ubd.Entries {
			ubd.Entries[i].CreationHeight = 0
		}
		app.Keepers.Cosmos.Staking.SetUnbondingDelegation(ctx, ubd)
		return false
	})

	// Iterate through validators by power descending, reset bond heights, and
	// update bond intra-tx counters.
	store := ctx.KVStore(app.GetKey(stakingtypes.StoreKey))
	iter := sdk.KVStoreReversePrefixIterator(store, stakingtypes.ValidatorsKey)
	counter := int16(0)

	for ; iter.Valid(); iter.Next() {
		addr := sdk.ValAddress(stakingtypes.AddressFromValidatorsKey(iter.Key()))
		validator, found := app.Keepers.Cosmos.Staking.GetValidator(ctx, addr)
		if !found {
			panic("expected validator, not found")
		}

		validator.UnbondingHeight = 0
		if applyAllowedAddrs && !allowedAddrsMap[addr.String()] {
			validator.Jailed = true
		}

		app.Keepers.Cosmos.Staking.SetValidator(ctx, validator)
		counter++
	}

	_ = iter.Close()

	_, _ = app.Keepers.Cosmos.Staking.ApplyAndReturnValidatorSetUpdates(ctx)

	/* Handle slashing state. */

	// reset start height on signing infos
	app.Keepers.Cosmos.Slashing.IterateValidatorSigningInfos(
		ctx,
		func(addr sdk.ConsAddress, info slashingtypes.ValidatorSigningInfo) (stop bool) {
			info.StartHeight = 0
			app.Keepers.Cosmos.Slashing.SetValidatorSigningInfo(ctx, addr, info)
			return false
		},
	)
}

// Setup initializes a new AkashApp. A Nop logger is set in AkashApp.
func Setup(opts ...SetupAppOption) *AkashApp {
	cfg := &setupAppOptions{
		encCfg:  MakeEncodingConfig(),
		home:    DefaultHome,
		checkTx: false,
		chainID: "akash-1",
	}

	for _, opt := range opts {
		opt(cfg)
	}

	db := dbm.NewMemDB()

	appOpts := viper.New()

	appOpts.Set("home", cfg.home)

	r := rand.New(rand.NewSource(0)) // nolint: gosec
	genTime := simulation.RandTimestamp(r)

	appOpts.Set("GenesisTime", genTime)

	app := NewApp(
		logger.NewNopLogger(),
		db,
		nil,
		true,
		5,
		map[int64]bool{},
		cfg.encCfg,
		appOpts,
		baseapp.SetChainID(cfg.chainID),
	)

	if !cfg.checkTx {
		var state GenesisState
		if cfg.genesisFn == nil {
			// init chain must be called to stop deliverState from being nil
			state = NewDefaultGenesisState(app.AppCodec())
		} else {
			state = cfg.genesisFn(app.cdc)
		}

		stateBytes, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			panic(err)
		}

		// Initialize the chain
		app.InitChain(
			abci.RequestInitChain{
				Validators:    []abci.ValidatorUpdate{},
				AppStateBytes: stateBytes,
				ChainId:       cfg.chainID,
			},
		)
	}

	return app
}

//
// func OptsWithGenesisTime(seed int64) servertypes.AppOptions {
// 	r := rand.New(rand.NewSource(seed)) // nolint: gosec
// 	genTime := simulation.RandTimestamp(r)
//
// 	appOpts := viper.New()
// 	appOpts.Set("GenesisTime", genTime)
// 	simapp.FlagGenesisTimeValue = genTime.Unix()
//
// 	return appOpts
// }
