package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	cmd "github.com/tendermint/tendermint/cmd/tendermint/commands"
	"github.com/tendermint/tendermint/cmd/tendermint/commands/debug"
	cfg "github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/libs/cli"
	tmflags "github.com/tendermint/tendermint/libs/cli/flags"
	"github.com/tendermint/tendermint/libs/log"
	tmos "github.com/tendermint/tendermint/libs/os"
	tmrand "github.com/tendermint/tendermint/libs/rand"
	"github.com/tendermint/tendermint/p2p"
	cs "github.com/tendermint/tendermint/test/maverick/consensus"
	nd "github.com/tendermint/tendermint/test/maverick/node"
	"github.com/tendermint/tendermint/types"
	tmtime "github.com/tendermint/tendermint/types/time"
)

var (
	config                   = cfg.DefaultConfig()
	logger                   = log.NewTMLogger(log.NewSyncWriter(os.Stdout))
	misbehaviorFlag          = ""
	failureSpecFile          = ""
	nodeIndex                = 0
	failureStartUnixMs int64 = 0
)

func init() {
	registerFlagsRootCmd(RootCmd)
}

func registerFlagsRootCmd(command *cobra.Command) {
	command.PersistentFlags().String("log_level", config.LogLevel, "Log level")
}

func ParseConfig() (*cfg.Config, error) {
	conf := cfg.DefaultConfig()
	err := viper.Unmarshal(conf)
	if err != nil {
		return nil, err
	}
	conf.SetRoot(conf.RootDir)
	cfg.EnsureRoot(conf.RootDir)
	if err = conf.ValidateBasic(); err != nil {
		return nil, fmt.Errorf("error in config file: %v", err)
	}
	return conf, err
}

// RootCmd is the root command for Tendermint core.
var RootCmd = &cobra.Command{
	Use:   "maverick",
	Short: "Tendermint Maverick Node",
	Long: "Tendermint Maverick Node for testing with faulty consensus misbehaviors in a testnet. Contains " +
		"all the functionality of a normal node but custom misbehaviors can be injected when running the node " +
		"through a flag. See maverick node --help for how the misbehavior flag is constructured",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) (err error) {
		fmt.Printf("use: %v, args: %v", cmd.Use, cmd.Args)

		config, err = ParseConfig()
		if err != nil {
			return err
		}

		if config.LogFormat == cfg.LogFormatJSON {
			logger = log.NewTMJSONLogger(log.NewSyncWriter(os.Stdout))
		}

		logger, err = tmflags.ParseLogLevel(config.LogLevel, logger, cfg.DefaultLogLevel)
		if err != nil {
			return err
		}

		if viper.GetBool(cli.TraceFlag) {
			logger = log.NewTracingLogger(logger)
		}

		logger = logger.With("module", "main")
		return nil
	},
}

func main() {
	rootCmd := RootCmd
	rootCmd.AddCommand(
		ListMisbehaviorCmd,
		cmd.GenValidatorCmd,
		InitFilesCmd,
		cmd.ProbeUpnpCmd,
		cmd.ReplayCmd,
		cmd.ReplayConsoleCmd,
		cmd.ResetAllCmd,
		cmd.ResetPrivValidatorCmd,
		cmd.ShowValidatorCmd,
		cmd.ShowNodeIDCmd,
		cmd.GenNodeKeyCmd,
		cmd.VersionCmd,
		debug.DebugCmd,
		cli.NewCompletionCmd(rootCmd, true),
	)

	nodeCmd := &cobra.Command{
		Use:   "node",
		Short: "Run the maverick node",
		RunE: func(command *cobra.Command, args []string) error {
			return startNode(config, logger, misbehaviorFlag, failureSpecFile, nodeIndex, failureStartUnixMs)
		},
	}

	cmd.AddNodeFlags(nodeCmd)

	// Create & start node
	rootCmd.AddCommand(nodeCmd)

	// add special flag for misbehaviors
	nodeCmd.Flags().StringVar(
		&misbehaviorFlag,
		"misbehaviors",
		"",
		"Select the misbehaviors of the node (comma-separated, no spaces in between): \n"+
			"e.g. --misbehaviors double-prevote,3\n"+
			"You can also have multiple misbehaviors: e.g. double-prevote,3,no-vote,5")

	// propose-delay failure spec flags
	nodeCmd.Flags().StringVar(
		&failureSpecFile,
		"failure-spec",
		"",
		"Path to failure_spec.xml defining phase-based Tendermint proposal delay.")
	nodeCmd.Flags().StringVar(
		&failureSpecFile,
		"delay-schedule",
		"",
		"Deprecated alias for --failure-spec (supports legacy JSON schedules).")
	nodeCmd.Flags().IntVar(
		&nodeIndex,
		"node-index",
		0,
		"Index of this node (0, 1, 2, ...) used for any node-specific schedule entries")
	nodeCmd.Flags().Int64Var(
		&failureStartUnixMs,
		"failure-start-unix-ms",
		0,
		"Absolute Unix timestamp in milliseconds used as start time for failure-spec warm-up/phases (0 = process start)")

	cmd := cli.PrepareBaseCmd(rootCmd, "TM", os.ExpandEnv(filepath.Join("$HOME", cfg.DefaultTendermintDir)))
	if err := cmd.Execute(); err != nil {
		panic(err)
	}
}

func startNode(config *cfg.Config, logger log.Logger, misbehaviorFlag, failureSpecFile string, nodeIndex int, failureStartUnixMs int64) error {
	if failureStartUnixMs < 0 {
		return fmt.Errorf("--failure-start-unix-ms must be >= 0")
	}

	misbehaviors, err := nd.ParseMisbehaviors(misbehaviorFlag)
	if err != nil {
		return err
	}

	if failureSpecFile != "" {
		mb, err := cs.ProposeDelayMisbehavior(failureSpecFile, nodeIndex, failureStartUnixMs)
		if err != nil {
			return fmt.Errorf("failed to load proposal delay spec: %w", err)
		}
		// Key 0 is a wildcard — active at all heights not covered by a
		// height-specific entry from --misbehaviors.
		if _, exists := misbehaviors[0]; !exists {
			misbehaviors[0] = mb
		}
	}

	node, err := nd.DefaultNewNode(config, logger, misbehaviors)
	if err != nil {
		return fmt.Errorf("failed to create node: %w", err)
	}

	if err := node.Start(); err != nil {
		return fmt.Errorf("failed to start node: %w", err)
	}

	logger.Info("Started node", "nodeInfo", node.Switch().NodeInfo())

	// Stop upon receiving SIGTERM or CTRL-C.
	tmos.TrapSignal(logger, func() {
		if node.IsRunning() {
			if err := node.Stop(); err != nil {
				logger.Error("unable to stop the node", "error", err)
			}
		}
	})

	// Run forever.
	select {}
}

var InitFilesCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize Tendermint",
	RunE:  initFiles,
}

func initFiles(cmd *cobra.Command, args []string) error {
	return initFilesWithConfig(config)
}

func initFilesWithConfig(config *cfg.Config) error {
	// private validator
	privValKeyFile := config.PrivValidatorKeyFile()
	privValStateFile := config.PrivValidatorStateFile()
	var pv *nd.FilePV
	if tmos.FileExists(privValKeyFile) {
		pv = nd.LoadFilePV(privValKeyFile, privValStateFile)
		logger.Info("Found private validator", "keyFile", privValKeyFile,
			"stateFile", privValStateFile)
	} else {
		pv = nd.GenFilePV(privValKeyFile, privValStateFile)
		pv.Save()
		logger.Info("Generated private validator", "keyFile", privValKeyFile,
			"stateFile", privValStateFile)
	}

	nodeKeyFile := config.NodeKeyFile()
	if tmos.FileExists(nodeKeyFile) {
		logger.Info("Found node key", "path", nodeKeyFile)
	} else {
		if _, err := p2p.LoadOrGenNodeKey(nodeKeyFile); err != nil {
			return err
		}
		logger.Info("Generated node key", "path", nodeKeyFile)
	}

	// genesis file
	genFile := config.GenesisFile()
	if tmos.FileExists(genFile) {
		logger.Info("Found genesis file", "path", genFile)
	} else {
		genDoc := types.GenesisDoc{
			ChainID:         fmt.Sprintf("test-chain-%v", tmrand.Str(6)),
			GenesisTime:     tmtime.Now(),
			ConsensusParams: types.DefaultConsensusParams(),
		}
		pubKey, err := pv.GetPubKey()
		if err != nil {
			return fmt.Errorf("can't get pubkey: %w", err)
		}
		genDoc.Validators = []types.GenesisValidator{{
			Address: pubKey.Address(),
			PubKey:  pubKey,
			Power:   10,
		}}

		if err := genDoc.SaveAs(genFile); err != nil {
			return err
		}
		logger.Info("Generated genesis file", "path", genFile)
	}

	return nil
}

var ListMisbehaviorCmd = &cobra.Command{
	Use:   "misbehaviors",
	Short: "Lists possible misbehaviors",
	RunE:  listMisbehaviors,
}

func listMisbehaviors(cmd *cobra.Command, args []string) error {
	str := "Currently registered misbehaviors: \n"
	for key := range cs.MisbehaviorList {
		str += fmt.Sprintf("- %s\n", key)
	}
	fmt.Println(str)
	return nil
}
