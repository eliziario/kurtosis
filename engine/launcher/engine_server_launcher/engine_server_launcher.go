/*
 * Copyright (c) 2021 - present Kurtosis Technologies Inc.
 * All Rights Reserved.
 */

package engine_server_launcher

import (
	"context"
	"github.com/kurtosis-tech/kurtosis/container-engine-lib/lib/backend_interface"
	"github.com/kurtosis-tech/kurtosis/container-engine-lib/lib/backend_interface/objects/port_spec"
	"github.com/kurtosis-tech/kurtosis/engine/launcher/args"
	"github.com/kurtosis-tech/kurtosis/kurtosis_version"
	"github.com/kurtosis-tech/stacktrace"
	"github.com/sirupsen/logrus"
	"net"
)

const (
	// TODO This should come from the same logic that builds the server image!!!!!
	containerImage = "kurtosistech/engine"
)

type EngineServerLauncher struct {
	kurtosisBackend backend_interface.KurtosisBackend
}

func NewEngineServerLauncher(kurtosisBackend backend_interface.KurtosisBackend) *EngineServerLauncher {
	return &EngineServerLauncher{kurtosisBackend: kurtosisBackend}
}

func (launcher *EngineServerLauncher) LaunchWithDefaultVersion(
	ctx context.Context,
	logLevel logrus.Level,
	grpcListenPortNum uint16, // The port that the engine server will listen on AND the port that it should be bound to on the host machine
	grpcProxyListenPortNum uint16, // Envoy proxy port that will forward grpc-web calls to the engine
	metricsUserID string,
	didUserAcceptSendingMetrics bool,
	backendConfigSupplier KurtosisBackendConfigSupplier,
) (
	resultPublicIpAddr net.IP,
	resultPublicGrpcPortSpec *port_spec.PortSpec,
	// NOTE: We can return a resultPublicGrpcProxyPortNum here if we ever need it
	resultErr error,
) {
	publicIpAddr, publicGrpcPortSpec, err := launcher.LaunchWithCustomVersion(
		ctx,
		kurtosis_version.KurtosisVersion,
		logLevel,
		grpcListenPortNum,
		grpcProxyListenPortNum,
		metricsUserID,
		didUserAcceptSendingMetrics,
		backendConfigSupplier,
	)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred launching the engine server container with default version tag '%v'", kurtosis_version.KurtosisVersion)
	}
	return publicIpAddr, publicGrpcPortSpec, nil
}

func (launcher *EngineServerLauncher) LaunchWithCustomVersion(
	ctx context.Context,
	imageVersionTag string,
	logLevel logrus.Level,
	grpcListenPortNum uint16, // The port that the engine server will listen on AND the port that it should be bound to on the host machine
	grpcProxyListenPortNum uint16, // Envoy proxy port that will forward grpc-web calls to the engine
	metricsUserID string,
	didUserAcceptSendingMetrics bool,
	backendConfigSupplier KurtosisBackendConfigSupplier,
) (
	resultPublicIpAddr net.IP,
	resultPublicGrpcPortSpec *port_spec.PortSpec,
	resultErr error,
) {
	kurtosisBackendType, kurtosisBackendConfig := backendConfigSupplier.getKurtosisBackendConfig()
	argsObj, err := args.NewEngineServerArgs(
		grpcListenPortNum,
		grpcProxyListenPortNum,
		logLevel.String(),
		imageVersionTag,
		metricsUserID,
		didUserAcceptSendingMetrics,
		kurtosisBackendType,
		kurtosisBackendConfig,
	)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred creating the engine server args")
	}

	envVars, err := args.GetEnvFromArgs(argsObj)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred generating the engine server's environment variables")
	}

	engine, err := launcher.kurtosisBackend.CreateEngine(
		ctx,
		containerImage,
		imageVersionTag,
		grpcListenPortNum,
		grpcProxyListenPortNum,
		envVars,
	)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred launching the engine server container with environment variables '%+v'", envVars)
	}
	return engine.GetPublicIPAddress(), engine.GetPublicGRPCPort(), nil
}
