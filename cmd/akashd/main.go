package main

import (
	"encoding/json"
	"io"

	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/client/debug"
	"github.com/cosmos/cosmos-sdk/client/flags"
	"github.com/cosmos/cosmos-sdk/server"
	"github.com/cosmos/cosmos-sdk/store"
	storetypes "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth"
	genutilcli "github.com/cosmos/cosmos-sdk/x/genutil/client/cli"
	"github.com/cosmos/cosmos-sdk/x/staking"
	"github.com/ovrclk/akash/app"
	"github.com/ovrclk/akash/cmd/common"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/libs/cli"
	"github.com/tendermint/tendermint/libs/log"
	tmtypes "github.com/tendermint/tendermint/types"
	dbm "github.com/tendermint/tm-db"
)

const flagInvCheckPeriod = "inv-check-period"

var invCheckPeriod uint

func main() {
	common.InitSDKConfig()

	cdc := app.MakeCodec()
	ctx := server.NewDefaultContext()

	root := &cobra.Command{
		Use:               "akashd",
		Long:              "Akash Daemon CLI Utility.\n\nAkash is a peer-to-peer marketplace for computing resources and \na deployment platform for heavily distributed applications. \nFind out more at https://akash.network",
		PersistentPreRunE: server.PersistentPreRunEFn(ctx),
	}

	root.AddCommand(
		genutilcli.InitCmd(ctx, cdc, app.ModuleBasics(), common.DefaultNodeHome()),

		genutilcli.CollectGenTxsCmd(ctx, cdc, auth.GenesisAccountIterator{}, common.DefaultNodeHome()),

		genutilcli.MigrateGenesisCmd(ctx, cdc),

		genutilcli.GenTxCmd(
			ctx, cdc,
			app.ModuleBasics(),
			staking.AppModuleBasic{},
			auth.GenesisAccountIterator{},
			common.DefaultNodeHome(),
			common.DefaultCLIHome(),
		),

		genutilcli.ValidateGenesisCmd(ctx, cdc, app.ModuleBasics()),
		AddGenesisAccountCmd(ctx, cdc, common.DefaultNodeHome(), common.DefaultCLIHome()),

		flags.NewCompletionCmd(root, true),
		debug.Cmd(cdc),
	)

	server.AddCommands(ctx, cdc, root, newApp, exportAppStateAndTMValidators)

	executor := cli.PrepareBaseCmd(root, "AKASHD", common.DefaultNodeHome())
	root.PersistentFlags().UintVar(&invCheckPeriod, flagInvCheckPeriod,
		0, "Assert registered invariants every N blocks")
	err := executor.Execute()
	if err != nil {
		panic(err)
	}

}

func newApp(logger log.Logger, db dbm.DB, tio io.Writer) abci.Application {
	var cache sdk.MultiStorePersistentCache

	if viper.GetBool(server.FlagInterBlockCache) {
		cache = store.NewCommitKVStoreCacheManager()
	}

	skipUpgradeHeights := make(map[int64]bool)
	for _, h := range viper.GetIntSlice(server.FlagUnsafeSkipUpgrades) {
		skipUpgradeHeights[int64(h)] = true
	}

	return app.NewApp(
		logger, db, tio, true, invCheckPeriod, skipUpgradeHeights,
		baseapp.SetPruning(storetypes.NewPruningOptionsFromString(viper.GetString("pruning"))),
		baseapp.SetMinGasPrices(viper.GetString(server.FlagMinGasPrices)),
		baseapp.SetHaltHeight(viper.GetUint64(server.FlagHaltHeight)),
		baseapp.SetHaltTime(viper.GetUint64(server.FlagHaltTime)),
		baseapp.SetInterBlockCache(cache),
	)
}

func exportAppStateAndTMValidators(
	logger log.Logger, db dbm.DB, tio io.Writer, height int64, forZeroHeight bool, jailWhiteList []string,
) (json.RawMessage, []tmtypes.GenesisValidator, error) {

	var akashApp *app.AkashApp
	if height != -1 {
		akashApp = app.NewApp(logger, db, tio, false, uint(1), map[int64]bool{})

		if err := akashApp.LoadHeight(height); err != nil {
			return nil, nil, err
		}
	} else {
		akashApp = app.NewApp(logger, db, tio, true, uint(1), map[int64]bool{})
	}

	return akashApp.ExportAppStateAndValidators(forZeroHeight, jailWhiteList)
}
