package cache

import (
	"context"

	"github.com/lavanet/lava/utils"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

const (
	FlagLogLevel       = "log_level"
	FlagMetricsAddress = "metrics_address"
)

func CreateCacheCobraCommand() *cobra.Command {
	cacheCmd := &cobra.Command{
		Use:   "cache [address<HOST:PORT>]",
		Short: "set up a ram based cache server for relays listening on address specified, can work either with rpcconsumer or rpcprovider to improve latency",
		Long: `set up a ram based cache server for relays listening on address specified, can work either with rpcconsumer or rpcprovider to improve latency,
longer DefaultExpirationForNonFinalized will reduce sync QoS for "latest" request but reduce load`,
		Example: `cache "127.0.0.1:7777"`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			address := args[0]

			ctx := context.Background()
			logLevel, err := cmd.Flags().GetString(FlagLogLevel)
			if err != nil {
				utils.LavaFormatFatal("failed to read log level flag", err)
			}
			utils.SetGlobalLoggingLevel(logLevel)

			metricsAddress, err := cmd.Flags().GetString(FlagMetricsAddress)
			if err != nil {
				utils.LavaFormatFatal("failed to read metrics address flag", err)
			}
			Server(ctx, address, metricsAddress, cmd.Flags())
			return nil
		},
	}
	cacheCmd.Flags().String(FlagLogLevel, zerolog.InfoLevel.String(), "The logging level (trace|debug|info|warn|error|fatal|panic)")
	cacheCmd.Flags().Duration(ExpirationFlagName, DefaultExpirationTimeFinalized, "how long does a cache entry lasts in the cache for a finalized entry")
	cacheCmd.Flags().Duration(ExpirationNonFinalizedFlagName, DefaultExpirationForNonFinalized, "how long does a cache entry lasts in the cache for a non finalized entry")
	cacheCmd.Flags().String(FlagMetricsAddress, DisabledFlagOption, "address to listen to prometheus metrics 127.0.0.1:5555, later you can curl http://127.0.0.1:5555/metrics")
	cacheCmd.Flags().Int64(FlagCacheSizeName, 2*1024*1024*1024, "the maximal amount of entries to save")
	cacheCmd.Flags().Bool(FlagUseMethodInApiSpecificCacheMetricsName, false, "use method in the cache specific api metric")
	return cacheCmd
}
