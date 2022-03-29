//
// Copyright 2021-2022, Offchain Labs, Inc. All rights reserved.
//

package arbnode

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/rpc"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/arbitrum"
	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	flag "github.com/spf13/pflag"

	"github.com/offchainlabs/nitro/arbos"
	"github.com/offchainlabs/nitro/arbos/arbosState"
	"github.com/offchainlabs/nitro/arbstate"
	"github.com/offchainlabs/nitro/arbutil"
	"github.com/offchainlabs/nitro/broadcastclient"
	"github.com/offchainlabs/nitro/broadcaster"
	"github.com/offchainlabs/nitro/das"
	"github.com/offchainlabs/nitro/solgen/go/bridgegen"
	"github.com/offchainlabs/nitro/solgen/go/challengegen"
	"github.com/offchainlabs/nitro/solgen/go/ospgen"
	"github.com/offchainlabs/nitro/solgen/go/rollupgen"
	"github.com/offchainlabs/nitro/statetransfer"
	"github.com/offchainlabs/nitro/validator"
)

type RollupAddresses struct {
	Bridge                 common.Address
	Inbox                  common.Address
	SequencerInbox         common.Address
	Rollup                 common.Address
	ValidatorUtils         common.Address
	ValidatorWalletCreator common.Address
	DeployedAt             uint64
}

func andTxSucceeded(ctx context.Context, l1client arbutil.L1Interface, txTimeout time.Duration, tx *types.Transaction, err error) error {
	if err != nil {
		return fmt.Errorf("error submitting tx: %w", err)
	}
	_, err = arbutil.EnsureTxSucceededWithTimeout(ctx, l1client, tx, txTimeout)
	if err != nil {
		return fmt.Errorf("error executing tx: %w", err)
	}
	return nil
}

func deployBridgeCreator(ctx context.Context, client arbutil.L1Interface, auth *bind.TransactOpts, txTimeout time.Duration) (common.Address, error) {
	bridgeTemplate, tx, _, err := bridgegen.DeployBridge(auth, client)
	err = andTxSucceeded(ctx, client, txTimeout, tx, err)
	if err != nil {
		return common.Address{}, fmt.Errorf("bridge deploy error: %w", err)
	}

	seqInboxTemplate, tx, _, err := bridgegen.DeploySequencerInbox(auth, client)
	err = andTxSucceeded(ctx, client, txTimeout, tx, err)
	if err != nil {
		return common.Address{}, fmt.Errorf("sequencer inbox deploy error: %w", err)
	}

	inboxTemplate, tx, _, err := bridgegen.DeployInbox(auth, client)
	err = andTxSucceeded(ctx, client, txTimeout, tx, err)
	if err != nil {
		return common.Address{}, fmt.Errorf("inbox deploy error: %w", err)
	}

	rollupEventBridgeTemplate, tx, _, err := rollupgen.DeployRollupEventBridge(auth, client)
	err = andTxSucceeded(ctx, client, txTimeout, tx, err)
	if err != nil {
		return common.Address{}, fmt.Errorf("rollup event bridge deploy error: %w", err)
	}

	outboxTemplate, tx, _, err := bridgegen.DeployOutbox(auth, client)
	err = andTxSucceeded(ctx, client, txTimeout, tx, err)
	if err != nil {
		return common.Address{}, fmt.Errorf("outbox deploy error: %w", err)
	}

	bridgeCreatorAddr, tx, bridgeCreator, err := rollupgen.DeployBridgeCreator(auth, client)
	err = andTxSucceeded(ctx, client, txTimeout, tx, err)
	if err != nil {
		return common.Address{}, fmt.Errorf("bridge creator deploy error: %w", err)
	}

	tx, err = bridgeCreator.UpdateTemplates(auth, bridgeTemplate, seqInboxTemplate, inboxTemplate, rollupEventBridgeTemplate, outboxTemplate)
	err = andTxSucceeded(ctx, client, txTimeout, tx, err)
	if err != nil {
		return common.Address{}, fmt.Errorf("bridge creator update templates error: %w", err)
	}

	return bridgeCreatorAddr, nil
}

func deployChallengeFactory(
	ctx context.Context,
	client arbutil.L1Interface,
	auth *bind.TransactOpts,
	txTimeout time.Duration,
) (common.Address, common.Address, error) {
	osp0, tx, _, err := ospgen.DeployOneStepProver0(auth, client)
	err = andTxSucceeded(ctx, client, txTimeout, tx, err)
	if err != nil {
		return common.Address{}, common.Address{}, fmt.Errorf("osp0 deploy error: %w", err)
	}

	ospMem, _, _, err := ospgen.DeployOneStepProverMemory(auth, client)
	err = andTxSucceeded(ctx, client, txTimeout, tx, err)
	if err != nil {
		return common.Address{}, common.Address{}, fmt.Errorf("ospMemory deploy error: %w", err)
	}

	ospMath, _, _, err := ospgen.DeployOneStepProverMath(auth, client)
	err = andTxSucceeded(ctx, client, txTimeout, tx, err)
	if err != nil {
		return common.Address{}, common.Address{}, fmt.Errorf("ospMath deploy error: %w", err)
	}

	ospHostIo, _, _, err := ospgen.DeployOneStepProverHostIo(auth, client)
	err = andTxSucceeded(ctx, client, txTimeout, tx, err)
	if err != nil {
		return common.Address{}, common.Address{}, fmt.Errorf("ospHostIo deploy error: %w", err)
	}

	ospEntryAddr, tx, _, err := ospgen.DeployOneStepProofEntry(auth, client, osp0, ospMem, ospMath, ospHostIo)
	err = andTxSucceeded(ctx, client, txTimeout, tx, err)
	if err != nil {
		return common.Address{}, common.Address{}, fmt.Errorf("ospEntry deploy error: %w", err)
	}

	challengeManagerAddr, tx, _, err := challengegen.DeployChallengeManager(auth, client)
	err = andTxSucceeded(ctx, client, txTimeout, tx, err)
	if err != nil {
		return common.Address{}, common.Address{}, fmt.Errorf("ospEntry deploy error: %w", err)
	}

	return ospEntryAddr, challengeManagerAddr, nil
}

func deployRollupCreator(ctx context.Context, client arbutil.L1Interface, auth *bind.TransactOpts, txTimeout time.Duration) (*rollupgen.RollupCreator, common.Address, error) {
	bridgeCreator, err := deployBridgeCreator(ctx, client, auth, txTimeout)
	if err != nil {
		return nil, common.Address{}, err
	}

	ospEntryAddr, challengeManagerAddr, err := deployChallengeFactory(ctx, client, auth, txTimeout)
	if err != nil {
		return nil, common.Address{}, err
	}

	rollupAdminLogic, tx, _, err := rollupgen.DeployRollupAdminLogic(auth, client)
	err = andTxSucceeded(ctx, client, txTimeout, tx, err)
	if err != nil {
		return nil, common.Address{}, fmt.Errorf("rollup admin logic deploy error: %w", err)
	}

	rollupUserLogic, tx, _, err := rollupgen.DeployRollupUserLogic(auth, client)
	err = andTxSucceeded(ctx, client, txTimeout, tx, err)
	if err != nil {
		return nil, common.Address{}, fmt.Errorf("rollup user logic deploy error: %w", err)
	}

	rollupCreatorAddress, tx, rollupCreator, err := rollupgen.DeployRollupCreator(auth, client)
	err = andTxSucceeded(ctx, client, txTimeout, tx, err)
	if err != nil {
		return nil, common.Address{}, fmt.Errorf("rollup user logic deploy error: %w", err)
	}

	tx, err = rollupCreator.SetTemplates(
		auth,
		bridgeCreator,
		ospEntryAddr,
		challengeManagerAddr,
		rollupAdminLogic,
		rollupUserLogic,
	)
	err = andTxSucceeded(ctx, client, txTimeout, tx, err)
	if err != nil {
		return nil, common.Address{}, fmt.Errorf("rollup user logic deploy error: %w", err)
	}

	return rollupCreator, rollupCreatorAddress, nil
}

func DeployOnL1(ctx context.Context, l1client arbutil.L1Interface, deployAuth *bind.TransactOpts, sequencer common.Address, authorizeValidators uint64, wasmModuleRoot common.Hash, chainId *big.Int, txTimeout time.Duration) (*RollupAddresses, error) {
	rollupCreator, rollupCreatorAddress, err := deployRollupCreator(ctx, l1client, deployAuth, txTimeout)
	if err != nil {
		return nil, fmt.Errorf("error deploying rollup creator: %w", err)
	}

	var confirmPeriodBlocks uint64 = 20
	var extraChallengeTimeBlocks uint64 = 200
	seqInboxParams := rollupgen.ISequencerInboxMaxTimeVariation{
		DelayBlocks:   big.NewInt(60 * 60 * 24 / 15),
		FutureBlocks:  big.NewInt(12),
		DelaySeconds:  big.NewInt(60 * 60 * 24),
		FutureSeconds: big.NewInt(60 * 60),
	}
	nonce, err := l1client.PendingNonceAt(ctx, rollupCreatorAddress)
	if err != nil {
		return nil, fmt.Errorf("error getting pending nonce: %w", err)
	}
	expectedRollupAddr := crypto.CreateAddress(rollupCreatorAddress, nonce+2)
	tx, err := rollupCreator.CreateRollup(
		deployAuth,
		rollupgen.Config{
			ConfirmPeriodBlocks:            confirmPeriodBlocks,
			ExtraChallengeTimeBlocks:       extraChallengeTimeBlocks,
			StakeToken:                     common.Address{},
			BaseStake:                      big.NewInt(params.Ether),
			WasmModuleRoot:                 wasmModuleRoot,
			Owner:                          deployAuth.From,
			LoserStakeEscrow:               common.Address{},
			ChainId:                        chainId,
			SequencerInboxMaxTimeVariation: seqInboxParams,
		},
		expectedRollupAddr,
	)
	if err != nil {
		return nil, fmt.Errorf("error submitting create rollup tx: %w", err)
	}
	receipt, err := arbutil.EnsureTxSucceededWithTimeout(ctx, l1client, tx, txTimeout)
	if err != nil {
		return nil, fmt.Errorf("error executing create rollup tx: %w", err)
	}
	info, err := rollupCreator.ParseRollupCreated(*receipt.Logs[len(receipt.Logs)-1])
	if err != nil {
		return nil, fmt.Errorf("error parsing rollup created log: %w", err)
	}

	rollup, err := rollupgen.NewRollupAdminLogic(info.RollupAddress, l1client)
	if err != nil {
		return nil, fmt.Errorf("error getting rollup admin: %w", err)
	}
	tx, err = rollup.SetIsBatchPoster(deployAuth, sequencer, true)
	err = andTxSucceeded(ctx, l1client, txTimeout, tx, err)
	if err != nil {
		return nil, fmt.Errorf("error setting is batch poster: %w", err)
	}

	validatorUtils, tx, _, err := rollupgen.DeployValidatorUtils(deployAuth, l1client)
	err = andTxSucceeded(ctx, l1client, txTimeout, tx, err)
	if err != nil {
		return nil, fmt.Errorf("validator utils deploy error: %w", err)
	}

	validatorWalletCreator, tx, _, err := rollupgen.DeployValidatorWalletCreator(deployAuth, l1client)
	err = andTxSucceeded(ctx, l1client, txTimeout, tx, err)
	if err != nil {
		return nil, fmt.Errorf("validator utils deploy error: %w", err)
	}

	var allowValidators []bool
	var validatorAddrs []common.Address
	for i := uint64(1); i <= authorizeValidators; i++ {
		validatorAddrs = append(validatorAddrs, crypto.CreateAddress(validatorWalletCreator, i))
		allowValidators = append(allowValidators, true)
	}
	if len(validatorAddrs) > 0 {
		tx, err = rollup.SetValidator(deployAuth, validatorAddrs, allowValidators)
		err = andTxSucceeded(ctx, l1client, txTimeout, tx, err)
		if err != nil {
			return nil, fmt.Errorf("error setting validator: %w", err)
		}
	}

	return &RollupAddresses{
		Bridge:                 info.DelayedBridge,
		Inbox:                  info.InboxAddress,
		SequencerInbox:         info.SequencerInbox,
		DeployedAt:             receipt.BlockNumber.Uint64(),
		Rollup:                 info.RollupAddress,
		ValidatorUtils:         validatorUtils,
		ValidatorWalletCreator: validatorWalletCreator,
	}, nil
}

type Config struct {
	RPC                  arbitrum.Config                `koanf:"rpc"`
	Sequencer            SequencerConfig                `koanf:"sequencer"`
	EnableL1Reader       bool                           `koanf:"enable-l1-reader"`
	InboxReader          InboxReaderConfig              `koanf:"inbox-reader"`
	DelayedSequencer     DelayedSequencerConfig         `koanf:"delayed-sequencer"`
	BatchPoster          BatchPosterConfig              `koanf:"batch-poster"`
	ForwardingTargetImpl string                         `koanf:"forwarding-target"`
	BlockValidator       validator.BlockValidatorConfig `koanf:"block-validator"`
	Feed                 broadcastclient.FeedConfig     `koanf:"feed"`
	Validator            validator.L1ValidatorConfig    `koanf:"validator"`
	SeqCoordinator       SeqCoordinatorConfig           `koanf:"seq-coordinator"`
	DataAvailability     das.DataAvailabilityConfig     `koanf:"data-availability"`
	Wasm                 WasmConfig                     `koanf:"wasm"`
	Dangerous            DangerousConfig                `koanf:"dangerous"`
	Archive              bool                           `koanf:"archive"`
}

func (c *Config) ForwardingTarget() string {
	if c.ForwardingTargetImpl == "null" {
		return ""
	}

	return c.ForwardingTargetImpl
}

func ConfigAddOptions(prefix string, f *flag.FlagSet, feedInputEnable bool, feedOutputEnable bool) {
	arbitrum.ConfigAddOptions(prefix+".rpc", f)
	SequencerConfigAddOptions(prefix+".sequencer", f)
	f.Bool(prefix+".enable-l1-reader", ConfigDefault.EnableL1Reader, "enable l1 reader")
	InboxReaderConfigAddOptions(prefix+".inbox-reader", f)
	DelayedSequencerConfigAddOptions(prefix+".delayed-sequencer", f)
	BatchPosterConfigAddOptions(prefix+".batch-poster", f)
	f.String(prefix+".forwarding-target", ConfigDefault.ForwardingTargetImpl, "transaction forwarding target URL, or \"null\" to disable forwarding (iff not sequencer)")
	validator.BlockValidatorConfigAddOptions(prefix+".block-validator", f)
	broadcastclient.FeedConfigAddOptions(prefix+".feed", f, feedInputEnable, feedOutputEnable)
	validator.L1ValidatorConfigAddOptions(prefix+".validator", f)
	SeqCoordinatorConfigAddOptions(prefix+".seq-coordinator", f)
	das.DataAvailabilityConfigAddOptions(prefix+".data-availability", f)
	WasmConfigAddOptions(prefix+".wasm", f)
	DangerousConfigAddOptions(prefix+".dangerous", f)
	f.Bool(prefix+".archive", ConfigDefault.Archive, "retain past block state")
}

var ConfigDefault = Config{
	RPC:                  arbitrum.DefaultConfig,
	Sequencer:            DefaultSequencerConfig,
	EnableL1Reader:       true,
	InboxReader:          DefaultInboxReaderConfig,
	DelayedSequencer:     DefaultDelayedSequencerConfig,
	BatchPoster:          DefaultBatchPosterConfig,
	ForwardingTargetImpl: "",
	BlockValidator:       validator.DefaultBlockValidatorConfig,
	Feed:                 broadcastclient.FeedConfigDefault,
	Validator:            validator.DefaultL1ValidatorConfig,
	SeqCoordinator:       DefaultSeqCoordinatorConfig,
	DataAvailability:     das.DefaultDataAvailabilityConfig,
	Wasm:                 DefaultWasmConfig,
	Dangerous:            DefaultDangerousConfig,
	Archive:              false,
}

func ConfigDefaultL1Test() *Config {
	config := ConfigDefault
	config.Sequencer = TestSequencerConfig
	config.InboxReader = TestInboxReaderConfig
	config.DelayedSequencer = TestDelayedSequencerConfig
	config.BatchPoster = TestBatchPosterConfig
	config.SeqCoordinator = TestSeqCoordinatorConfig

	return &config
}

func ConfigDefaultL2Test() *Config {
	config := ConfigDefault
	config.Sequencer = TestSequencerConfig
	config.EnableL1Reader = false
	config.SeqCoordinator = TestSeqCoordinatorConfig

	return &config
}

type DangerousConfig struct {
	NoL1Listener bool `koanf:"no-l1-listener"`
}

var DefaultDangerousConfig = DangerousConfig{
	NoL1Listener: false,
}

func DangerousConfigAddOptions(prefix string, f *flag.FlagSet) {
	f.Bool(prefix+".no-l1-listener", DefaultDangerousConfig.NoL1Listener, "DANGEROUS! disables listening to L1. To be used in test nodes only")
}

type DangerousSequencerConfig struct {
	NoCoordinator bool `koanf:"no-coordinator"`
}

var DefaultDangerousSequencerConfig = DangerousSequencerConfig{
	NoCoordinator: false,
}

var TestDangerousSequencerConfig = DangerousSequencerConfig{
	NoCoordinator: true,
}

func DangerousSequencerConfigAddOptions(prefix string, f *flag.FlagSet) {
	f.Bool(prefix+".no-coordinator", DefaultDangerousSequencerConfig.NoCoordinator, "DANGEROUS! allows sequencer without coordinator.")
}

type SequencerConfig struct {
	Enable    bool                     `koanf:"enable"`
	Dangerous DangerousSequencerConfig `koanf:"dangerous"`
}

var DefaultSequencerConfig = SequencerConfig{
	Enable:    false,
	Dangerous: DefaultDangerousSequencerConfig,
}

var TestSequencerConfig = SequencerConfig{
	Enable:    true,
	Dangerous: TestDangerousSequencerConfig,
}

func SequencerConfigAddOptions(prefix string, f *flag.FlagSet) {
	f.Bool(prefix+".enable", DefaultSequencerConfig.Enable, "act and post to l1 as sequencer")
	DangerousSequencerConfigAddOptions(prefix+".dangerous", f)
}

type WasmConfig struct {
	RootPath   string `koanf:"root-path"`
	ModuleRoot string `koanf:"module-root"`
	CachePath  string `koanf:"cache-path"`
}

func WasmConfigAddOptions(prefix string, f *flag.FlagSet) {
	f.String(prefix+".root-path", DefaultWasmConfig.RootPath, "path to wasm files (replay.wasm, wasi_stub.wasm, soft-float.wasm, go_stub.wasm, host_io.wasm, brotli.wasm")
	f.String(prefix+".module-root", DefaultWasmConfig.RootPath, "wasm module root (if empty, read from <wasmrootpath>/module_root)")
	f.String(prefix+".cache-path", DefaultWasmConfig.RootPath, "path for cache of wasm machines")
}

var DefaultWasmConfig = WasmConfig{
	RootPath:   "",
	ModuleRoot: "",
	CachePath:  "",
}

type Node struct {
	Backend          *arbitrum.Backend
	ArbInterface     *ArbInterface
	TxStreamer       *TransactionStreamer
	TxPublisher      TransactionPublisher
	DeployInfo       *RollupAddresses
	InboxReader      *InboxReader
	InboxTracker     *InboxTracker
	DelayedSequencer *DelayedSequencer
	BatchPoster      *BatchPoster
	BlockValidator   *validator.BlockValidator
	Staker           *validator.Staker
	BroadcastServer  *broadcaster.Broadcaster
	BroadcastClients []*broadcastclient.BroadcastClient
	SeqCoordinator   *SeqCoordinator
}

func createNodeImpl(stack *node.Node, chainDb ethdb.Database, config *Config, l2BlockChain *core.BlockChain, l1client arbutil.L1Interface, deployInfo *RollupAddresses, txOpts *bind.TransactOpts) (*Node, error) {
	var broadcastServer *broadcaster.Broadcaster
	if config.Feed.Output.Enable {
		broadcastServer = broadcaster.NewBroadcaster(config.Feed.Output)
	}

	dataAvailabilityMode, err := config.DataAvailability.Mode()
	if err != nil {
		return nil, err
	}
	var dataAvailabilityService das.DataAvailabilityService
	if dataAvailabilityMode == das.LocalDataAvailability {
		var err error
		dataAvailabilityService, err = das.NewLocalDiskDataAvailabilityService(config.DataAvailability.LocalDiskDataDir)
		if err != nil {
			return nil, err
		}
	}

	txStreamer, err := NewTransactionStreamer(chainDb, l2BlockChain, broadcastServer)
	if err != nil {
		return nil, err
	}
	var txPublisher TransactionPublisher
	var coordinator *SeqCoordinator
	var sequencer *Sequencer
	if config.Sequencer.Enable {
		if config.ForwardingTarget() != "" {
			return nil, errors.New("sequencer and forwarding target both set")
		}
		if !(config.SeqCoordinator.Enable || config.Sequencer.Dangerous.NoCoordinator) {
			return nil, errors.New("sequencer must be enabled with coordinator, unless dangerous.no-coordinator set")
		}
		if config.EnableL1Reader {
			if l1client == nil {
				return nil, errors.New("l1client is nil")
			}
			sequencer, err = NewSequencer(txStreamer, l1client)
		} else {
			sequencer, err = NewSequencer(txStreamer, nil)
		}
		if err != nil {
			return nil, err
		}
		txPublisher = sequencer
	} else {
		if config.DelayedSequencer.Enable {
			return nil, errors.New("cannot have delayedsequencer without sequencer")
		}
		if config.ForwardingTarget() == "" {
			txPublisher = NewTxDropper()
		} else {
			txPublisher = NewForwarder(config.ForwardingTarget())
		}
	}
	if config.SeqCoordinator.Enable {
		coordinator, err = NewSeqCoordinator(txStreamer, sequencer, config.SeqCoordinator)
		if err != nil {
			return nil, err
		}
	}
	arbInterface, err := NewArbInterface(txStreamer, txPublisher)
	if err != nil {
		return nil, err
	}
	backend, err := arbitrum.NewBackend(stack, &config.RPC, chainDb, arbInterface)
	if err != nil {
		return nil, err
	}

	var broadcastClients []*broadcastclient.BroadcastClient
	if config.Feed.Input.Enable() {
		for _, address := range config.Feed.Input.URLs {
			broadcastClients = append(broadcastClients, broadcastclient.NewBroadcastClient(address, nil, config.Feed.Input.Timeout, txStreamer))
		}
	}
	if !config.EnableL1Reader {
		return &Node{backend, arbInterface, txStreamer, txPublisher, nil, nil, nil, nil, nil, nil, nil, broadcastServer, broadcastClients, coordinator}, nil
	}

	if deployInfo == nil {
		return nil, errors.New("deployinfo is nil")
	}
	delayedBridge, err := NewDelayedBridge(l1client, deployInfo.Bridge, deployInfo.DeployedAt)
	if err != nil {
		return nil, err
	}
	sequencerInbox, err := NewSequencerInbox(l1client, deployInfo.SequencerInbox, int64(deployInfo.DeployedAt))
	if err != nil {
		return nil, err
	}
	inboxTracker, err := NewInboxTracker(chainDb, txStreamer, dataAvailabilityService)
	if err != nil {
		return nil, err
	}
	inboxReader, err := NewInboxReader(inboxTracker, l1client, new(big.Int).SetUint64(deployInfo.DeployedAt), delayedBridge, sequencerInbox, &(config.InboxReader))
	if err != nil {
		return nil, err
	}

	var blockValidator *validator.BlockValidator
	if config.BlockValidator.Enable {
		blockValidator, err = validator.NewBlockValidator(inboxReader, inboxTracker, txStreamer, l2BlockChain, rawdb.NewTable(chainDb, blockValidatorPrefix), &config.BlockValidator, dataAvailabilityService)
		if err != nil {
			return nil, err
		}
	}

	var staker *validator.Staker
	if config.Validator.Enable {
		// TODO: remember validator wallet in JSON instead of querying it from L1 every time
		wallet, err := validator.NewValidatorWallet(nil, deployInfo.ValidatorWalletCreator, deployInfo.Rollup, l1client, txOpts, int64(deployInfo.DeployedAt), func(common.Address) {})
		if err != nil {
			return nil, err
		}
		staker, err = validator.NewStaker(l1client, wallet, bind.CallOpts{}, config.Validator, l2BlockChain, inboxReader, inboxTracker, txStreamer, blockValidator, deployInfo.ValidatorUtils)
		if err != nil {
			return nil, err
		}
	}

	var batchPoster *BatchPoster
	var delayedSequencer *DelayedSequencer
	if config.BatchPoster.Enable {
		if txOpts == nil {
			return nil, errors.New("batchposter, but no TxOpts")
		}
		batchPoster, err = NewBatchPoster(l1client, inboxTracker, txStreamer, &config.BatchPoster, deployInfo.SequencerInbox, common.Address{}, txOpts, dataAvailabilityService)
		if err != nil {
			return nil, err
		}
	}
	if config.DelayedSequencer.Enable {
		delayedSequencer, err = NewDelayedSequencer(l1client, inboxReader, txStreamer, coordinator, &(config.DelayedSequencer))
		if err != nil {
			return nil, err
		}
	} else if !config.Sequencer.Enable {
		return nil, errors.New("sequencer and l1 reader, without delayed sequencer")
	}

	return &Node{backend, arbInterface, txStreamer, txPublisher, deployInfo, inboxReader, inboxTracker, delayedSequencer, batchPoster, blockValidator, staker, broadcastServer, broadcastClients, coordinator}, nil
}

type arbNodeLifecycle struct {
	node *Node
}

func (l arbNodeLifecycle) Start() error {
	return l.node.Start(context.Background())
}

func (l arbNodeLifecycle) Stop() error {
	l.node.StopAndWait()
	return nil
}

func CreateNode(stack *node.Node, chainDb ethdb.Database, config *Config, l2BlockChain *core.BlockChain, l1client arbutil.L1Interface, deployInfo *RollupAddresses, txOpts *bind.TransactOpts) (newNode *Node, err error) {
	node, err := createNodeImpl(stack, chainDb, config, l2BlockChain, l1client, deployInfo, txOpts)
	if err != nil {
		return nil, err
	}
	var apis []rpc.API
	if node.BlockValidator != nil {
		apis = append(apis, rpc.API{
			Namespace: "arb",
			Version:   "1.0",
			Service:   &BlockValidatorAPI{val: node.BlockValidator, blockchain: l2BlockChain},
			Public:    false,
		})
	}
	stack.RegisterAPIs(apis)

	stack.RegisterLifecycle(arbNodeLifecycle{node})
	return node, nil
}

func (n *Node) Start(ctx context.Context) error {
	err := n.TxPublisher.Initialize(ctx)
	if err != nil {
		return err
	}
	err = n.TxStreamer.Initialize()
	if err != nil {
		return err
	}
	if n.InboxTracker != nil {
		err = n.InboxTracker.Initialize()
		if err != nil {
			return err
		}
	}
	n.TxStreamer.Start(ctx)
	if n.InboxReader != nil {
		err = n.InboxReader.Start(ctx)
		if err != nil {
			return err
		}
	}
	err = n.TxPublisher.Start(ctx)
	if err != nil {
		return err
	}
	if n.SeqCoordinator != nil {
		n.SeqCoordinator.Start(ctx)
	}
	if n.DelayedSequencer != nil {
		n.DelayedSequencer.Start(ctx)
	}
	if n.BatchPoster != nil {
		n.BatchPoster.Start(ctx)
	}
	if n.BlockValidator != nil {
		err = n.BlockValidator.Start(ctx)
		if err != nil {
			return err
		}
	}
	if n.Staker != nil {
		err = n.Staker.Initialize(ctx)
		if err != nil {
			return err
		}
		n.Staker.Start(ctx)
	}
	if n.BroadcastServer != nil {
		err = n.BroadcastServer.Start(ctx)
		if err != nil {
			return err
		}
	}
	for _, client := range n.BroadcastClients {
		client.Start(ctx)
	}
	return nil
}

func (n *Node) StopAndWait() {
	for _, client := range n.BroadcastClients {
		client.StopAndWait()
	}
	if n.BroadcastServer != nil {
		n.BroadcastServer.StopAndWait()
	}
	if n.BlockValidator != nil {
		n.BlockValidator.StopAndWait()
	}
	if n.BatchPoster != nil {
		n.BatchPoster.StopAndWait()
	}
	if n.DelayedSequencer != nil {
		n.DelayedSequencer.StopAndWait()
	}
	if n.InboxReader != nil {
		n.InboxReader.StopAndWait()
	}
	n.TxPublisher.StopAndWait()
	if n.SeqCoordinator != nil {
		n.SeqCoordinator.StopAndWait()
	}
	n.TxStreamer.StopAndWait()
	n.ArbInterface.BlockChain().Stop()
}

func CreateDefaultStack() (*node.Node, error) {
	stackConf := node.DefaultConfig
	var err error
	stackConf.DataDir = ""
	stackConf.HTTPHost = "localhost"
	stackConf.HTTPModules = append(stackConf.HTTPModules, "eth")
	stack, err := node.New(&stackConf)
	if err != nil {
		return nil, fmt.Errorf("error creating protocol stack: %w", err)
	}
	return stack, nil
}

func DefaultCacheConfigFor(stack *node.Node, archiveMode bool) *core.CacheConfig {
	baseConf := ethconfig.Defaults
	if archiveMode {
		baseConf = ethconfig.ArchiveDefaults
	}

	return &core.CacheConfig{
		TrieCleanLimit:      baseConf.TrieCleanCache,
		TrieCleanJournal:    stack.ResolvePath(baseConf.TrieCleanCacheJournal),
		TrieCleanRejournal:  baseConf.TrieCleanCacheRejournal,
		TrieCleanNoPrefetch: baseConf.NoPrefetch,
		TrieDirtyLimit:      baseConf.TrieDirtyCache,
		TrieDirtyDisabled:   baseConf.NoPruning,
		TrieTimeLimit:       baseConf.TrieTimeout,
		SnapshotLimit:       baseConf.SnapshotCache,
		Preimages:           baseConf.Preimages,
	}
}

func ImportBlocksToChainDb(chainDb ethdb.Database, initDataReader statetransfer.StoredBlockReader) (uint64, error) {
	var prevHash common.Hash
	td := big.NewInt(0)
	var blocksInDb uint64
	if initDataReader.More() {
		var err error
		blocksInDb, err = chainDb.Ancients()
		if err != nil {
			return 0, err
		}
	}
	blockNum := uint64(0)
	for ; initDataReader.More(); blockNum++ {
		log.Debug("importing", "blockNum", blockNum)
		storedBlock, err := initDataReader.GetNext()
		if err != nil {
			return blockNum, err
		}
		if blockNum+1 < blocksInDb && initDataReader.More() {
			continue // skip already-imported blocks. Only validate the last.
		}
		storedBlockHash := storedBlock.Header.Hash()
		if blockNum < blocksInDb {
			// validate db and import match
			hashInDb := rawdb.ReadCanonicalHash(chainDb, blockNum)
			if storedBlockHash != hashInDb {
				utils.Fatalf("Import and Database disagree on hashes import: %v, Db: %v", storedBlockHash, hashInDb)
			}
		}
		if blockNum+1 == blocksInDb && blockNum > 0 {
			// we skipped blocks common to DB an import
			prevHash = rawdb.ReadCanonicalHash(chainDb, blockNum-1)
			td = rawdb.ReadTd(chainDb, prevHash, blockNum-1)
		}
		if storedBlock.Header.ParentHash != prevHash {
			utils.Fatalf("Import Block %d, parent hash %v, expected %v", blockNum, storedBlock.Header.ParentHash, prevHash)
		}
		if storedBlock.Header.Number.Cmp(new(big.Int).SetUint64(blockNum)) != 0 {
			panic("unexpected block number in import")
		}
		txs := types.Transactions{}
		for _, txData := range storedBlock.Transactions {
			tx := types.ArbitrumLegacyFromTransactionResult(txData)
			if tx.Hash() != txData.Hash {
				return blockNum, errors.New("bad txHash")
			}
			txs = append(txs, tx)
		}
		receipts := storedBlock.Reciepts
		block := types.NewBlockWithHeader(&storedBlock.Header).WithBody(txs, nil) // don't recalculate hashes
		blockHash := block.Hash()
		if blockHash != storedBlock.Header.Hash() {
			return blockNum, errors.New("bad blockHash")
		}
		_, err = rawdb.WriteAncientBlocks(chainDb, []*types.Block{block}, []types.Receipts{receipts}, td)
		if err != nil {
			return blockNum, err
		}
		prevHash = blockHash
		td.Add(td, storedBlock.Header.Difficulty)
	}
	return blockNum, initDataReader.Close()
}

func WriteOrTestGenblock(chainDb ethdb.Database, initData statetransfer.InitDataReader, blockNumber uint64, chainConfig *params.ChainConfig) error {
	arbstate.RequireHookedGeth()

	EmptyHash := common.Hash{}

	prevHash := EmptyHash
	prevDifficulty := big.NewInt(0)
	storedGenHash := rawdb.ReadCanonicalHash(chainDb, blockNumber)
	timestamp := uint64(0)
	if blockNumber > 0 {
		prevHash = rawdb.ReadCanonicalHash(chainDb, blockNumber-1)
		if prevHash == EmptyHash {
			return fmt.Errorf("block number %d not found in database", chainDb)
		}
		prevHeader := rawdb.ReadHeader(chainDb, prevHash, blockNumber-1)
		if prevHeader == nil {
			return fmt.Errorf("block header for block %d not found in database", chainDb)
		}
		timestamp = prevHeader.Time
	}
	stateRoot, err := arbosState.InitializeArbosInDatabase(chainDb, initData, chainConfig)
	if err != nil {
		return err
	}

	genBlock := arbosState.MakeGenesisBlock(prevHash, blockNumber, timestamp, stateRoot)
	blockHash := genBlock.Hash()

	if storedGenHash == EmptyHash {
		// chainDb did not have genesis block. Initialize it.
		core.WriteHeadBlock(chainDb, genBlock, prevDifficulty)
	} else if storedGenHash != blockHash {
		return errors.New("database contains data inconsistent with initialization")
	}

	return nil
}

func WriteOrTestChainConfig(chainDb ethdb.Database, config *params.ChainConfig) error {
	EmptyHash := common.Hash{}

	block0Hash := rawdb.ReadCanonicalHash(chainDb, 0)
	if block0Hash == EmptyHash {
		return errors.New("block 0 not found")
	}
	storedConfig := rawdb.ReadChainConfig(chainDb, block0Hash)
	if storedConfig == nil {
		rawdb.WriteChainConfig(chainDb, block0Hash, config)
		return nil
	}
	height := rawdb.ReadHeaderNumber(chainDb, rawdb.ReadHeadHeaderHash(chainDb))
	if height == nil {
		return errors.New("non empty chain config but empty chain")
	}
	err := storedConfig.CheckCompatible(config, *height)
	if err != nil {
		return err
	}
	rawdb.WriteChainConfig(chainDb, block0Hash, config)
	return nil
}

func GetBlockChain(chainDb ethdb.Database, cacheConfig *core.CacheConfig, config *params.ChainConfig) (*core.BlockChain, error) {
	defaultConf := ethconfig.Defaults

	engine := arbos.Engine{
		IsSequencer: true,
	}

	vmConfig := vm.Config{
		EnablePreimageRecording: defaultConf.EnablePreimageRecording,
	}

	return core.NewBlockChain(chainDb, cacheConfig, config, engine, vmConfig, shouldPreserveFalse, &defaultConf.TxLookupLimit)
}

func WriteOrTestBlockChain(chainDb ethdb.Database, cacheConfig *core.CacheConfig, initData statetransfer.InitDataReader, blockNumber uint64, config *params.ChainConfig) (*core.BlockChain, error) {
	err := WriteOrTestGenblock(chainDb, initData, blockNumber, config)
	if err != nil {
		return nil, err
	}
	err = WriteOrTestChainConfig(chainDb, config)
	if err != nil {
		return nil, err
	}
	return GetBlockChain(chainDb, cacheConfig, config)
}

// Don't preserve reorg'd out blocks
func shouldPreserveFalse(header *types.Header) bool {
	return false
}
