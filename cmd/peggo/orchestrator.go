package main

import (
	"context"
	"os"
	"time"

	ethcmn "github.com/ethereum/go-ethereum/common"
	cli "github.com/jawher/mow.cli"
	rpchttp "github.com/tendermint/tendermint/rpc/client/http"
	"github.com/xlab/closer"
	log "github.com/xlab/suplog"

	"github.com/InjectiveLabs/sdk-go/chain/peggy/types"
	chainclient "github.com/InjectiveLabs/sdk-go/client/chain"
	"github.com/InjectiveLabs/sdk-go/client/common"

	"github.com/InjectiveLabs/peggo/orchestrator"
	"github.com/InjectiveLabs/peggo/orchestrator/coingecko"
	"github.com/InjectiveLabs/peggo/orchestrator/cosmos"
	"github.com/InjectiveLabs/peggo/orchestrator/cosmos/tmclient"
	"github.com/InjectiveLabs/peggo/orchestrator/ethereum/committer"
	"github.com/InjectiveLabs/peggo/orchestrator/ethereum/peggy"
	"github.com/InjectiveLabs/peggo/orchestrator/ethereum/provider"
	"github.com/InjectiveLabs/peggo/orchestrator/relayer"

	ctypes "github.com/InjectiveLabs/sdk-go/chain/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// startOrchestrator action runs an infinite loop,
// listening for events and performing hooks.
//
// $ peggo orchestrator
func orchestratorCmd(cmd *cli.Cmd) {
	// orchestrator-specific CLI options
	var (
		// Cosmos params
		cosmosChainID   *string
		cosmosGRPC      *string
		tendermintRPC   *string
		cosmosGasPrices *string

		// Cosmos Key Management
		cosmosKeyringDir     *string
		cosmosKeyringAppName *string
		cosmosKeyringBackend *string

		cosmosKeyFrom       *string
		cosmosKeyPassphrase *string
		cosmosPrivKey       *string
		cosmosUseLedger     *bool

		// Ethereum params
		ethChainID            *int
		ethNodeRPC            *string
		ethNodeAlchemyWS      *string
		ethGasPriceAdjustment *float64
		ethMaxGasPrice        *string

		// Ethereum Key Management
		ethKeystoreDir *string
		ethKeyFrom     *string
		ethPassphrase  *string
		ethPrivKey     *string
		ethUseLedger   *bool

		// Relayer config
		relayValsets          *bool
		relayValsetOffsetDur  *string
		relayBatches          *bool
		relayBatchOffsetDur   *string
		pendingTxWaitDuration *string

		// Batch requester config
		minBatchFeeUSD *float64

		periodicBatchRequesting *bool

		coingeckoApi *string
	)

	initCosmosOptions(
		cmd,
		&cosmosChainID,
		&cosmosGRPC,
		&tendermintRPC,
		&cosmosGasPrices,
	)

	initCosmosKeyOptions(
		cmd,
		&cosmosKeyringDir,
		&cosmosKeyringAppName,
		&cosmosKeyringBackend,
		&cosmosKeyFrom,
		&cosmosKeyPassphrase,
		&cosmosPrivKey,
		&cosmosUseLedger,
	)

	initEthereumOptions(
		cmd,
		&ethChainID,
		&ethNodeRPC,
		&ethNodeAlchemyWS,
		&ethGasPriceAdjustment,
		&ethMaxGasPrice,
	)

	initEthereumKeyOptions(
		cmd,
		&ethKeystoreDir,
		&ethKeyFrom,
		&ethPassphrase,
		&ethPrivKey,
		&ethUseLedger,
	)

	initRelayerOptions(
		cmd,
		&relayValsets,
		&relayValsetOffsetDur,
		&relayBatches,
		&relayBatchOffsetDur,
		&pendingTxWaitDuration,
	)

	initBatchRequesterOptions(
		cmd,
		&minBatchFeeUSD,
		&periodicBatchRequesting,
	)

	initCoingeckoOptions(
		cmd,
		&coingeckoApi,
	)

	cmd.Before = func() {
		initMetrics(cmd)
	}

	cmd.Action = func() {
		// ensure a clean exit
		defer closer.Close()

		if *cosmosUseLedger || *ethUseLedger {
			log.Fatalln("cannot really use Ledger for orchestrator, since signatures msut be realtime")
		}

		valAddress, cosmosKeyring, err := initCosmosKeyring(
			cosmosKeyringDir,
			cosmosKeyringAppName,
			cosmosKeyringBackend,
			cosmosKeyFrom,
			cosmosKeyPassphrase,
			cosmosPrivKey,
			cosmosUseLedger,
		)
		if err != nil {
			log.WithError(err).Fatalln("failed to init Cosmos keyring")
		}

		ethKeyFromAddress, signerFn, personalSignFn, err := initEthereumAccountsManager(
			uint64(*ethChainID),
			ethKeystoreDir,
			ethKeyFrom,
			ethPassphrase,
			ethPrivKey,
			ethUseLedger,
		)
		if err != nil {
			log.WithError(err).Fatalln("failed to init Ethereum account")
		}

		log.Infoln("Using Cosmos ValAddress", valAddress.String())
		log.Infoln("Using Ethereum address", ethKeyFromAddress.String())

		clientCtx, err := chainclient.NewClientContext(*cosmosChainID, valAddress.String(), cosmosKeyring)
		if err != nil {
			log.WithError(err).Fatalln("failed to initialize cosmos client context")
		}
		clientCtx = clientCtx.WithNodeURI(*tendermintRPC)
		tmRPC, err := rpchttp.New(*tendermintRPC, "/websocket")
		if err != nil {
			log.WithError(err)
		}
		clientCtx = clientCtx.WithClient(tmRPC)

		daemonClient, err := chainclient.NewChainClient(clientCtx, *cosmosGRPC, common.OptionGasPrices(*cosmosGasPrices))
		if err != nil {
			log.WithError(err).WithFields(log.Fields{
				"endpoint": *cosmosGRPC,
			}).Fatalln("failed to connect to daemon, is injectived running?")
		}

		log.Infoln("Waiting for injectived GRPC")
		time.Sleep(1 * time.Second)

		daemonWaitCtx, cancelWait := context.WithTimeout(context.Background(), time.Minute)
		grpcConn := daemonClient.QueryClient()
		waitForService(daemonWaitCtx, grpcConn)
		peggyQuerier := types.NewQueryClient(grpcConn)
		peggyBroadcaster := cosmos.NewPeggyBroadcastClient(
			peggyQuerier,
			daemonClient,
			signerFn,
			personalSignFn,
		)
		cancelWait()

		// Query peggy params
		cosmosQueryClient := cosmos.NewPeggyQueryClient(peggyQuerier)
		ctx, cancelFn := context.WithCancel(context.Background())
		closer.Bind(cancelFn)

		peggyParams, err := cosmosQueryClient.PeggyParams(ctx)
		if err != nil {
			log.WithError(err).Fatalln("failed to query peggy params, is injectived running?")
		}

		peggyAddress := ethcmn.HexToAddress(peggyParams.BridgeEthereumAddress)
		injAddress := ethcmn.HexToAddress(peggyParams.CosmosCoinErc20Contract)

		// Check if the provided ETH address belongs to a validator
		isValidator, err := isValidatorAddress(cosmosQueryClient, ethKeyFromAddress)
		if err != nil {
			log.WithError(err).Fatalln("failed to query the current validator set from injective")

			return
		}

		erc20ContractMapping := make(map[ethcmn.Address]string)
		erc20ContractMapping[injAddress] = ctypes.InjectiveCoin

		evmRPC, err := rpc.Dial(*ethNodeRPC)
		if err != nil {
			log.WithField("endpoint", *ethNodeRPC).WithError(err).Fatalln("Failed to connect to Ethereum RPC")
			return
		}
		ethProvider := provider.NewEVMProvider(evmRPC)
		log.Infoln("Connected to Ethereum RPC at", *ethNodeRPC)

		ethCommitter, err := committer.NewEthCommitter(ethKeyFromAddress, *ethGasPriceAdjustment, *ethMaxGasPrice, signerFn, ethProvider)
		orShutdown(err)

		pendingTxInputList := peggy.PendingTxInputList{}

		pendingTxWaitDuration, err := time.ParseDuration(*pendingTxWaitDuration)
		orShutdown(err)

		peggyContract, err := peggy.NewPeggyContract(ethCommitter, peggyAddress, pendingTxInputList, pendingTxWaitDuration)
		orShutdown(err)

		// If Alchemy Websocket URL is set, then Subscribe to Pending Transaction of Peggy Contract.
		if *ethNodeAlchemyWS != "" {
			go peggyContract.SubscribeToPendingTxs(*ethNodeAlchemyWS)
		}

		relayer := relayer.NewPeggyRelayer(cosmosQueryClient, tmclient.NewRPCClient(*tendermintRPC), peggyContract, *relayValsets, *relayValsetOffsetDur, *relayBatches, *relayBatchOffsetDur)

		coingeckoConfig := coingecko.Config{
			BaseURL: *coingeckoApi,
		}
		coingeckoFeed := coingecko.NewCoingeckoPriceFeed(100, &coingeckoConfig)

		// make the flag obsolete and hardcode
		*minBatchFeeUSD = 49.0

		svc := orchestrator.NewPeggyOrchestrator(
			cosmosQueryClient,
			peggyBroadcaster,
			tmclient.NewRPCClient(*tendermintRPC),
			peggyContract,
			ethKeyFromAddress,
			signerFn,
			personalSignFn,
			erc20ContractMapping,
			relayer,
			*minBatchFeeUSD,
			coingeckoFeed,
			*periodicBatchRequesting,
		)

		go func() {
			if err := svc.Start(ctx, isValidator); err != nil {
				log.Errorln(err)

				// signal there that the app failed
				os.Exit(1)
			}
		}()

		closer.Hold()
	}
}

func isValidatorAddress(peggyQuery cosmos.PeggyQueryClient, addr ethcmn.Address) (bool, error) {
	ctx, cancelFn := context.WithTimeout(context.Background(), time.Second*30)
	defer cancelFn()

	currentValset, err := peggyQuery.CurrentValset(ctx)
	if err != nil {
		return false, err
	}

	var isValidator bool
	for _, validator := range currentValset.Members {
		if ethcmn.HexToAddress(validator.EthereumAddress) == addr {
			isValidator = true
		}
	}

	return isValidator, nil
}
