package add

import (
	"context"
	"fmt"
	"github.com/kurtosis-tech/kurtosis/api/golang/engine/kurtosis_engine_rpc_api_bindings"
	enclave_consts "github.com/kurtosis-tech/kurtosis/api/golang/engine/lib/enclave"
	"github.com/kurtosis-tech/kurtosis/cli/cli/command_framework/highlevel/engine_consuming_kurtosis_command"
	"github.com/kurtosis-tech/kurtosis/cli/cli/command_framework/lowlevel/args"
	"github.com/kurtosis-tech/kurtosis/cli/cli/command_framework/lowlevel/flags"
	"github.com/kurtosis-tech/kurtosis/cli/cli/command_str_consts"
	"github.com/kurtosis-tech/kurtosis/cli/cli/defaults"
	"github.com/kurtosis-tech/kurtosis/cli/cli/helpers/engine_manager"
	"github.com/kurtosis-tech/kurtosis/cli/cli/helpers/logrus_log_levels"
	"github.com/kurtosis-tech/kurtosis/cli/cli/helpers/output_printers"
	"github.com/kurtosis-tech/kurtosis/container-engine-lib/lib/backend_interface"
	metrics_client "github.com/kurtosis-tech/metrics-library/golang/lib/client"
	"github.com/kurtosis-tech/stacktrace"
	"github.com/sirupsen/logrus"
	"strings"
)

const (
	apiContainerVersionFlagKey  = "api-container-version"
	apiContainerLogLevelFlagKey = "api-container-log-level"
	isSubnetworksEnabledFlagKey = "with-subnetworks"
	// TODO(deprecation) remove enclave ids in favor of names
	enclaveIdFlagKey   = "id"
	enclaveNameFlagKey = "name"

	defaultIsSubnetworksEnabled = "false"

	// Signifies that an enclave ID should be auto-generated
	autogenerateEnclaveIdKeyword = ""

	// Signifies that an enclave name should be auto-generated
	autogenerateEnclaveNameKeyword = ""

	kurtosisBackendCtxKey = "kurtosis-backend"
	engineClientCtxKey    = "engine-client"
)

// EnclaveAddCmd Suppressing exhaustruct requirement because this struct has ~40 properties
// nolint: exhaustruct
var EnclaveAddCmd = &engine_consuming_kurtosis_command.EngineConsumingKurtosisCommand{
	CommandStr:                command_str_consts.EnclaveAddCmdStr,
	ShortDescription:          "Creates an enclave",
	LongDescription:           "Creates a new, empty Kurtosis enclave",
	RunFunc:                   run,
	KurtosisBackendContextKey: kurtosisBackendCtxKey,
	EngineClientContextKey:    engineClientCtxKey,
	Flags: []*flags.FlagConfig{
		{
			Key:       apiContainerLogLevelFlagKey,
			Shorthand: "l",
			Type:      flags.FlagType_String,
			Usage: fmt.Sprintf(
				"The log level that the API container should log at (%v)",
				strings.Join(logrus_log_levels.GetAcceptableLogLevelStrs(), "|"),
			),
			Default: defaults.DefaultApiContainerLogLevel.String(),
		}, {
			Key:       apiContainerVersionFlagKey,
			Shorthand: "a",
			Type:      flags.FlagType_String,
			Default:   defaults.DefaultAPIContainerVersion,
			Usage:     "The version of the Kurtosis API container that should be started inside the enclave (blank tells the engine to use the default version)",
		}, {
			Key:       isSubnetworksEnabledFlagKey,
			Shorthand: "p",
			Type:      flags.FlagType_Bool,
			Default:   defaultIsSubnetworksEnabled,
			Usage:     "If set to true then the enclave that gets created will have subnetwork capabilities",
		}, {
			Key:       enclaveIdFlagKey,
			Shorthand: "i",
			Default:   autogenerateEnclaveIdKeyword,
			Usage: fmt.Sprintf(
				"The enclave ID to give the new enclave, which must match regex '%v' "+
					"(emptystring will autogenerate an enclave ID). Note this will be deprecated in favor of '%v'",
				enclave_consts.AllowedEnclaveNameCharsRegexStr,
				enclaveNameFlagKey,
			),
			Type: flags.FlagType_String,
		}, {
			Key:       enclaveNameFlagKey,
			Shorthand: "n",
			Default:   autogenerateEnclaveNameKeyword,
			Usage: fmt.Sprintf(
				"The enclave name to give the new enclave, which must match regex '%v' "+
					"(emptystring will autogenerate an enclave name)",
				enclave_consts.AllowedEnclaveNameCharsRegexStr,
			),
			Type: flags.FlagType_String,
		},
	},
}

func run(
	ctx context.Context,
	_ backend_interface.KurtosisBackend,
	_ kurtosis_engine_rpc_api_bindings.EngineServiceClient,
	metricsClient metrics_client.MetricsClient,
	flags *flags.ParsedFlags,
	_ *args.ParsedArgs,
) error {

	apiContainerVersion, err := flags.GetString(apiContainerVersionFlagKey)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred while getting the API Container Version using flag with key '%v'; this is a bug in Kurtosis", apiContainerVersionFlagKey)
	}

	isPartitioningEnabled, err := flags.GetBool(isSubnetworksEnabledFlagKey)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred getting the is-subnetwork-enabled setting using flag key '%v'; this is a bug in Kurtosis", isSubnetworksEnabledFlagKey)
	}

	kurtosisLogLevelStr, err := flags.GetString(apiContainerLogLevelFlagKey)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred while getting the API Container log level using flag with key '%v'; this is a bug in Kurtosis", apiContainerLogLevelFlagKey)
	}

	enclaveIdStr, err := flags.GetString(enclaveIdFlagKey)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred while getting the enclave id using flag with key '%v'; this is a bug in Kurtosis ", enclaveIdFlagKey)
	}

	enclaveName, err := flags.GetString(enclaveNameFlagKey)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred while getting the enclave name using flag with key '%v'; this is a bug in Kurtosis ", enclaveNameFlagKey)
	}

	engineManager, err := engine_manager.NewEngineManager(ctx)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred creating an engine manager.")
	}
	engineClient, closeClientFunc, err := engineManager.StartEngineIdempotentlyWithDefaultVersion(ctx, defaults.DefaultEngineLogLevel)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred creating a new Kurtosis engine client")
	}
	defer func() {
		if err = closeClientFunc(); err != nil {
			logrus.Errorf("Error closing the engine client")
		}
	}()

	logrus.Info("Creating new enclave...")

	// if the enclave id is provider but name isn't we go with the supplied id
	// TODO deprecate ids
	if enclaveIdStr != autogenerateEnclaveIdKeyword && enclaveName == autogenerateEnclaveNameKeyword {
		enclaveName = enclaveIdStr
	}

	if err = metricsClient.TrackCreateEnclave(enclaveName); err != nil {
		logrus.Warn("An error occurred while logging the create enclave event")
	}

	createEnclaveArgs := &kurtosis_engine_rpc_api_bindings.CreateEnclaveArgs{
		EnclaveName:            enclaveName,
		ApiContainerVersionTag: apiContainerVersion,
		ApiContainerLogLevel:   kurtosisLogLevelStr,
		IsPartitioningEnabled:  isPartitioningEnabled,
	}
	createdEnclaveResponse, err := engineClient.CreateEnclave(ctx, createEnclaveArgs)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred creating an enclave with ID '%v'", enclaveIdStr)
	}

	enclaveInfo := createdEnclaveResponse.GetEnclaveInfo()
	enclaveName = enclaveInfo.Name

	defer output_printers.PrintEnclaveName(enclaveName)

	return nil
}
