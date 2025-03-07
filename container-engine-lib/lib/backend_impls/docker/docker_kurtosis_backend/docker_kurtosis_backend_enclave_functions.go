package docker_kurtosis_backend

import (
	"context"
	"encoding/json"
	"fmt"
	docker_types "github.com/docker/docker/api/types"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/gammazero/workerpool"
	"github.com/kurtosis-tech/kurtosis/container-engine-lib/lib/backend_impls/docker/docker_kurtosis_backend/consts"
	"github.com/kurtosis-tech/kurtosis/container-engine-lib/lib/backend_impls/docker/docker_manager"
	"github.com/kurtosis-tech/kurtosis/container-engine-lib/lib/backend_impls/docker/docker_manager/types"
	"github.com/kurtosis-tech/kurtosis/container-engine-lib/lib/backend_impls/docker/docker_operation_parallelizer"
	"github.com/kurtosis-tech/kurtosis/container-engine-lib/lib/backend_impls/docker/object_attributes_provider/label_key_consts"
	"github.com/kurtosis-tech/kurtosis/container-engine-lib/lib/backend_impls/docker/object_attributes_provider/label_value_consts"
	"github.com/kurtosis-tech/kurtosis/container-engine-lib/lib/backend_interface/objects/enclave"
	"github.com/kurtosis-tech/stacktrace"
	"github.com/sirupsen/logrus"
	"io"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"time"
)

const (
	shouldFetchStoppedContainersWhenGettingEnclaveStatus = true

	containerInspectResultFilename = "spec.json"
	containerLogsFilename          = "output.log"

	shouldFetchStoppedContainersWhenDumpingEnclave = true
	numContainersToDumpAtOnce                      = 20

	// Permissions for the files & directories we create as a result of the dump
	createdDirPerms  = 0755
	createdFilePerms = 0644

	shouldFollowContainerLogsWhenDumping = false

	containerSpecJsonSerializationIndent = "  "
	containerSpecJsonSerializationPrefix = ""
)

// TODO: MIGRATE THIS FOLDER TO USE STRUCTURE OF USER_SERVICE_FUNCTIONS MODULE

type matchingNetworkInformation struct {
	enclaveUuid   enclave.EnclaveUUID
	enclaveStatus enclave.EnclaveStatus
	dockerNetwork *types.Network
	containers    []*types.Container
}

func (backend *DockerKurtosisBackend) CreateEnclave(ctx context.Context, enclaveUuid enclave.EnclaveUUID, enclaveName string, isPartitioningEnabled bool) (*enclave.Enclave, error) {
	teardownCtx := context.Background() // Separate context for tearing stuff down in case the input context is cancelled

	searchNetworkLabels := map[string]string{
		label_key_consts.AppIDDockerLabelKey.GetString():       label_value_consts.AppIDDockerLabelValue.GetString(),
		label_key_consts.EnclaveUUIDDockerLabelKey.GetString(): string(enclaveUuid),
	}

	networks, err := backend.dockerManager.GetNetworksByLabels(ctx, searchNetworkLabels)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting networks using labels '%+v', which is necessary to ensure that our enclave doesn't exist yet", searchNetworkLabels)
	}
	if len(networks) > 0 {
		return nil, stacktrace.NewError("Cannot create enclave with ID '%v' because an enclave with ID '%v' already exists", enclaveUuid, enclaveUuid)
	}

	volumeSearchLabels := map[string]string{
		label_key_consts.AppIDDockerLabelKey.GetString():       label_value_consts.AppIDDockerLabelValue.GetString(),
		label_key_consts.EnclaveUUIDDockerLabelKey.GetString(): string(enclaveUuid),
		label_key_consts.VolumeTypeDockerLabelKey.GetString():  label_value_consts.EnclaveDataVolumeTypeDockerLabelValue.GetString(),
	}
	foundVolumes, err := backend.dockerManager.GetVolumesByLabels(ctx, volumeSearchLabels)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting enclave data volumes matching labels '%+v'", volumeSearchLabels)
	}
	if len(foundVolumes) > 0 {
		return nil, stacktrace.NewError("Cannot create enclave with ID '%v' because one or more enclave data volume for that enclave already exists", enclaveUuid)
	}

	enclaveObjAttrsProvider, err := backend.objAttrsProvider.ForEnclave(enclaveUuid)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred while trying to generate an object attributes provider for the enclave with ID '%v'", enclaveUuid)
	}

	creationTime := time.Now()

	enclaveNetworkAttrs, err := enclaveObjAttrsProvider.ForEnclaveNetwork(enclaveName, creationTime, isPartitioningEnabled)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred while trying to get the enclave network attributes for the enclave with ID '%v'", enclaveUuid)
	}

	enclaveDataVolumeAttrs, err := enclaveObjAttrsProvider.ForEnclaveDataVolume()
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred while trying to get the enclave data volume attributes for the enclave with ID '%v'", enclaveUuid)
	}

	enclaveNetworkName := enclaveNetworkAttrs.GetName()
	enclaveNetworkDockerObjectLabels := enclaveNetworkAttrs.GetLabels()

	enclaveNetworkLabels := map[string]string{}
	for dockerLabelKey, dockerLabelValue := range enclaveNetworkDockerObjectLabels {
		enclaveNetworkLabelKey := dockerLabelKey.GetString()
		enclaveNetworkLabelValue := dockerLabelValue.GetString()
		enclaveNetworkLabels[enclaveNetworkLabelKey] = enclaveNetworkLabelValue
	}

	logrus.Debugf("Creating Docker network for enclave '%v'...", enclaveUuid)
	networkId, err := backend.dockerNetworkAllocator.CreateNewNetwork(
		ctx,
		enclaveNetworkName.GetString(),
		enclaveNetworkLabels,
	)
	if err != nil {
		// TODO If the user Ctrl-C's while the CreateNetwork call is ongoing then the CreateNetwork will error saying
		//  that the Context was cancelled as expected, but *the Docker engine will still create the network*!!! We'll
		//  need to parse the log message for the string "context canceled" and, if found, do another search for
		//  networks with our network name and delete them
		return nil, stacktrace.Propagate(err, "An error occurred allocating a new network for enclave '%v'", enclaveUuid)
	}
	shouldDeleteNetwork := true
	defer func() {
		if shouldDeleteNetwork {
			if err := backend.dockerManager.RemoveNetwork(teardownCtx, networkId); err != nil {
				logrus.Errorf("Creating the enclave didn't complete successfully, so we tried to delete network '%v' that we created but an error was thrown:\n%v", networkId, err)
				logrus.Errorf("ACTION REQUIRED: You'll need to manually remove network with ID '%v'!!!!!!!", networkId)
			}
		}
	}()
	logrus.Debugf("Docker network '%v' created successfully with ID '%v'", enclaveUuid, networkId)

	enclaveDataVolumeNameStr := enclaveDataVolumeAttrs.GetName().GetString()
	enclaveDataVolumeLabelStrs := map[string]string{}
	for labelKey, labelValue := range enclaveDataVolumeAttrs.GetLabels() {
		enclaveDataVolumeLabelStrs[labelKey.GetString()] = labelValue.GetString()
	}
	if err := backend.dockerManager.CreateVolume(ctx, enclaveDataVolumeNameStr, enclaveDataVolumeLabelStrs); err != nil {
		return nil, stacktrace.Propagate(
			err,
			"An error occurred creating enclave data volume with name '%v' and labels '%+v'",
			enclaveDataVolumeNameStr,
			enclaveDataVolumeLabelStrs,
		)
	}
	shouldDeleteVolume := true
	defer func() {
		if shouldDeleteVolume {
			if err := backend.dockerManager.RemoveVolume(teardownCtx, enclaveDataVolumeNameStr); err != nil {
				logrus.Errorf(
					"Creating the enclave didn't complete successfully, so we tried to delete enclave data volume '%v' "+
						"that we created but an error was thrown:\n%v",
					enclaveDataVolumeNameStr,
					err,
				)
				logrus.Errorf("ACTION REQUIRED: You'll need to manually remove volume with name '%v'!!!!!!!", enclaveDataVolumeNameStr)
			}
		}
	}()

	newEnclave := enclave.NewEnclave(enclaveUuid, enclaveName, enclave.EnclaveStatus_Empty, &creationTime)

	shouldDeleteNetwork = false
	shouldDeleteVolume = false
	return newEnclave, nil
}

// Gets enclaves matching the given filters
func (backend *DockerKurtosisBackend) GetEnclaves(
	ctx context.Context,
	filters *enclave.EnclaveFilters,
) (
	map[enclave.EnclaveUUID]*enclave.Enclave,
	error,
) {

	allMatchingNetworkInfo, err := backend.getMatchingEnclaveNetworkInfo(ctx, filters)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting enclave networks matching filters '%+v'", filters)
	}

	result := map[enclave.EnclaveUUID]*enclave.Enclave{}
	for enclaveUuid, matchingNetworkInfo := range allMatchingNetworkInfo {

		creationTime, err := getEnclaveCreationTimeFromNetwork(matchingNetworkInfo.dockerNetwork)
		if err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred getting the enclave's creation time value from enclave's Docker network '%+v'", matchingNetworkInfo.dockerNetwork)
		}

		enclaveName := getEnclaveNameFromNetwork(matchingNetworkInfo.dockerNetwork)

		result[enclaveUuid] = enclave.NewEnclave(
			enclaveUuid,
			enclaveName,
			matchingNetworkInfo.enclaveStatus,
			creationTime,
		)
	}

	return result, nil
}

// Stops enclaves matching the given filters
func (backend *DockerKurtosisBackend) StopEnclaves(
	ctx context.Context,
	filters *enclave.EnclaveFilters,
) (
	resultSuccessfulEnclaveUuids map[enclave.EnclaveUUID]bool,
	resultErroredEnclaveUuids map[enclave.EnclaveUUID]error,
	resultErr error,
) {

	matchingNetworkInfo, err := backend.getMatchingEnclaveNetworkInfo(ctx, filters)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred getting enclave network info using filters '%+v'", filters)
	}

	// For all the enclaves to stop, gather all the containers that should be stopped
	enclaveUuidsForContainerIdsToStop := map[string]enclave.EnclaveUUID{}
	containerIdsToStop := map[string]bool{}
	for enclaveUuid, networkInfo := range matchingNetworkInfo {
		for _, container := range networkInfo.containers {
			containerId := container.GetId()
			enclaveUuidsForContainerIdsToStop[containerId] = enclaveUuid
			containerIdsToStop[containerId] = true
		}
	}

	var stopEnclaveContainerOperation docker_operation_parallelizer.DockerOperation = func(ctx context.Context, dockerManager *docker_manager.DockerManager, dockerObjectId string) error {
		if err := dockerManager.KillContainer(ctx, dockerObjectId); err != nil {
			return stacktrace.Propagate(err, "An error occurred killing enclave container with ID '%v'", dockerObjectId)
		}
		return nil
	}

	_, erroredContainerIds := docker_operation_parallelizer.RunDockerOperationInParallel(
		ctx,
		containerIdsToStop,
		backend.dockerManager,
		stopEnclaveContainerOperation,
	)

	// Do we need to explicitly wait until the containers exit?

	containerKillErrorStrsByEnclave := map[enclave.EnclaveUUID][]string{}
	for erroredContainerId, killContainerErr := range erroredContainerIds {
		containerEnclaveUuid, found := enclaveUuidsForContainerIdsToStop[erroredContainerId]
		if !found {
			return nil, nil, stacktrace.NewError("An error occurred stopping container '%v' in an enclave we didn't request", erroredContainerId)
		}

		existingEnclaveErrors, found := containerKillErrorStrsByEnclave[containerEnclaveUuid]
		if !found {
			existingEnclaveErrors = []string{}
		}
		containerKillErrorStrsByEnclave[containerEnclaveUuid] = append(existingEnclaveErrors, killContainerErr.Error())
	}

	erroredEnclaveUuids := map[enclave.EnclaveUUID]error{}
	successfulEnclaveUuids := map[enclave.EnclaveUUID]bool{}
	for enclaveUuid := range matchingNetworkInfo {
		containerRemovalErrorStrs, found := containerKillErrorStrsByEnclave[enclaveUuid]
		if !found || len(containerRemovalErrorStrs) == 0 {
			successfulEnclaveUuids[enclaveUuid] = true
			continue
		}

		errorStr := strings.Join(containerRemovalErrorStrs, "\n\n")
		erroredEnclaveUuids[enclaveUuid] = stacktrace.NewError(
			"One or more errors occurred killing the containers in enclave '%v':\n%v",
			enclaveUuid,
			errorStr,
		)
	}

	return successfulEnclaveUuids, erroredEnclaveUuids, nil
}

func (backend *DockerKurtosisBackend) DumpEnclave(
	ctx context.Context,
	enclaveUuid enclave.EnclaveUUID,
	outputDirpath string,
) error {
	enclaveContainerSearchLabels := map[string]string{
		label_key_consts.AppIDDockerLabelKey.GetString():       label_value_consts.AppIDDockerLabelValue.GetString(),
		label_key_consts.EnclaveUUIDDockerLabelKey.GetString(): string(enclaveUuid),
	}

	enclaveContainers, err := backend.dockerManager.GetContainersByLabels(ctx, enclaveContainerSearchLabels, shouldFetchStoppedContainersWhenDumpingEnclave)
	if err != nil {
		return stacktrace.Propagate(
			err,
			"An error occurred getting the containers in enclave '%v' for dumping the enclave",
			enclaveUuid,
		)
	}

	// Create output directory
	if _, err := os.Stat(outputDirpath); !os.IsNotExist(err) {
		return stacktrace.NewError("Cannot create output directory at '%v'; directory already exists", outputDirpath)
	}
	if err := os.Mkdir(outputDirpath, createdDirPerms); err != nil {
		return stacktrace.Propagate(err, "An error occurred creating output directory at '%v'", outputDirpath)
	}

	workerPool := workerpool.New(numContainersToDumpAtOnce)
	resultErrsChan := make(chan error, len(enclaveContainers))
	for _, container := range enclaveContainers {
		containerName := container.GetName()
		containerId := container.GetId()
		logrus.Debugf("Submitting job to dump info about container with name '%v' and ID '%v'", containerName, containerId)

		/*
			!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!! WARNING !!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
			It's VERY important that the actual `func()` job function get created inside a helper function!!
			This is because variables declared inside for-loops are created BY REFERENCE rather than by-value, which
				means that if we inline the `func() {....}` creation here then all the job functions would get a REFERENCE to
				any variables they'd use.
			This means that by the time the job functions were run in the worker pool (long after the for-loop finished)
				then all the job functions would be using a reference from the last iteration of the for-loop.

			For more info, see the "Variables declared in for loops are passed by reference" section of:
				https://www.calhoun.io/gotchas-and-common-mistakes-with-closures-in-go/ for more details
			!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!! WARNING !!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
		*/
		jobToSubmit := createDumpContainerJob(
			ctx,
			backend.dockerManager,
			outputDirpath,
			resultErrsChan,
			containerName,
			containerId,
		)

		workerPool.Submit(jobToSubmit)
	}
	workerPool.StopWait()
	close(resultErrsChan)

	allResultErrStrs := []string{}
	for resultErr := range resultErrsChan {
		allResultErrStrs = append(allResultErrStrs, resultErr.Error())
	}

	if len(allResultErrStrs) > 0 {
		allIndexedResultErrStrs := []string{}
		for idx, resultErrStr := range allResultErrStrs {
			indexedResultErrStr := fmt.Sprintf(">>>>>>>>>>>>>>>>> ERROR %v <<<<<<<<<<<<<<<<<\n%v", idx, resultErrStr)
			allIndexedResultErrStrs = append(allIndexedResultErrStrs, indexedResultErrStr)
		}

		// NOTE: We don't use stacktrace here because the actual stacktraces we care about are the ones from the threads!
		return fmt.Errorf("The following errors occurred when trying to dump information about enclave '%v':\n%v",
			enclaveUuid,
			strings.Join(allIndexedResultErrStrs, "\n\n"))
	}
	return nil
}

// Destroys enclaves matching the given filters
func (backend *DockerKurtosisBackend) DestroyEnclaves(
	ctx context.Context,
	filters *enclave.EnclaveFilters,
) (
	resultSuccessfulEnclaveUuids map[enclave.EnclaveUUID]bool,
	resultErroredEnclaveUuids map[enclave.EnclaveUUID]error,
	resultErr error,
) {

	matchingNetworkInfo, err := backend.getMatchingEnclaveNetworkInfo(ctx, filters)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred getting enclave network info using filters '%+v'", filters)
	}

	// TODO Remove this check once the KurtosisBackend functions have been divvied up to the places that use them (e.g.
	//  API container gets service stuff, engine gets enclave stuff, etc.)
	for enclaveUuid := range matchingNetworkInfo {
		if _, found := backend.enclaveFreeIpProviders[enclaveUuid]; found {
			return nil, nil, stacktrace.NewError(
				"Received a request to destroy enclave '%v' for which a free IP address tracker is registered; this likely "+
					"means that destroy enclave is being called where it shouldn't be (i.e. inside the API container)",
				enclaveUuid,
			)
		}
		if _, found := backend.serviceRegistrations[enclaveUuid]; found {
			return nil, nil, stacktrace.NewError(
				"Received a request to destroy enclave '%v' for which services are being tracked; this likely "+
					"means that destroy enclave is being called where it shouldn't be (i.e. inside the API container)",
				enclaveUuid,
			)
		}
	}

	erroredEnclaveUuids := map[enclave.EnclaveUUID]error{}

	successfulContainerRemovalEnclaveUuids, erroredContainerRemovalEnclaveUuids, err := destroyContainersInEnclaves(ctx, backend.dockerManager, matchingNetworkInfo)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred destroying containers in enclaves matching filters '%+v'", filters)
	}
	for enclaveUuid, containerRemovalErr := range erroredContainerRemovalEnclaveUuids {
		erroredEnclaveUuids[enclaveUuid] = containerRemovalErr
	}

	successfulVolumeRemovalEnclaveUuids, erroredVolumeRemovalEnclaveUuids, err := destroyVolumesInEnclaves(ctx, backend.dockerManager, successfulContainerRemovalEnclaveUuids)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred destroying volumes in enclaves for which containers were successfully destroyed: %+v", successfulContainerRemovalEnclaveUuids)
	}
	for enclaveUuid, volumeRemovalErr := range erroredVolumeRemovalEnclaveUuids {
		erroredEnclaveUuids[enclaveUuid] = volumeRemovalErr
	}

	// Remove the networks
	networksToDestroy := map[enclave.EnclaveUUID]string{}
	for enclaveUuid := range successfulVolumeRemovalEnclaveUuids {
		networkInfo, found := matchingNetworkInfo[enclaveUuid]
		if !found {
			return nil, nil, stacktrace.NewError("Would have attempted to destroy enclave '%v' that didn't match the filters", enclaveUuid)
		}
		networksToDestroy[enclaveUuid] = networkInfo.dockerNetwork.GetId()
	}
	successfulNetworkRemovalEnclaveUuids, erroredNetworkRemovalEnclaveUuids, err := destroyEnclaveNetworks(ctx, backend.dockerManager, networksToDestroy)
	if err != nil {
		return nil, nil, stacktrace.Propagate(err, "An error occurred destroying the networks for enclaves whose volumes were successfully destroyed: %+v", successfulVolumeRemovalEnclaveUuids)
	}
	for enclaveUuid, networkRemovalErr := range erroredNetworkRemovalEnclaveUuids {
		erroredEnclaveUuids[enclaveUuid] = networkRemovalErr
	}

	return successfulNetworkRemovalEnclaveUuids, erroredEnclaveUuids, nil
}

// ====================================================================================================
//
//	Private helper methods
//
// ====================================================================================================
func (backend *DockerKurtosisBackend) getMatchingEnclaveNetworkInfo(
	ctx context.Context,
	filters *enclave.EnclaveFilters,
) (
	// Keyed by network ID
	map[enclave.EnclaveUUID]*matchingNetworkInformation,
	error,
) {
	kurtosisNetworkLabels := map[string]string{
		label_key_consts.AppIDDockerLabelKey.GetString(): label_value_consts.AppIDDockerLabelValue.GetString(),
		// NOTE: we don't search by enclave ID here because Docker has no way to do disjunctive search
	}

	allKurtosisNetworks, err := backend.dockerManager.GetNetworksByLabels(ctx, kurtosisNetworkLabels)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting Kurtosis networks")
	}

	// First, filter by enclave UUIDs
	matchingKurtosisEnclaveUuidsByNetworkId := map[enclave.EnclaveUUID]*types.Network{}
	for _, kurtosisNetwork := range allKurtosisNetworks {
		enclaveUuid, err := getEnclaveUuidFromNetwork(kurtosisNetwork)
		if err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred getting enclave ID from network '%+v'; this is a bug in Kurtosis", kurtosisNetwork)
		}

		if filters.UUIDs != nil && len(filters.UUIDs) > 0 {
			if _, found := filters.UUIDs[enclaveUuid]; !found {
				continue
			}
		}

		matchingKurtosisEnclaveUuidsByNetworkId[enclaveUuid] = kurtosisNetwork
	}

	// Next, filter by enclave status
	result := map[enclave.EnclaveUUID]*matchingNetworkInformation{}
	for enclaveUuid, kurtosisNetwork := range matchingKurtosisEnclaveUuidsByNetworkId {
		status, containers, err := backend.getEnclaveStatusAndContainers(ctx, enclaveUuid)
		if err != nil {
			return nil, stacktrace.Propagate(err, "An error occurred getting enclave status and containers from network for enclave '%v'", enclaveUuid)
		}

		if filters.Statuses != nil && len(filters.Statuses) > 0 {
			if _, found := filters.Statuses[status]; !found {
				continue
			}
		}

		result[enclaveUuid] = &matchingNetworkInformation{
			enclaveUuid:   enclaveUuid,
			enclaveStatus: status,
			dockerNetwork: kurtosisNetwork,
			containers:    containers,
		}
	}

	return result, nil
}

func (backend *DockerKurtosisBackend) getEnclaveStatusAndContainers(
	ctx context.Context,
	enclaveUuid enclave.EnclaveUUID,
) (

	enclave.EnclaveStatus,
	[]*types.Container,
	error,
) {
	allEnclaveContainers, err := backend.getAllEnclaveContainers(ctx, enclaveUuid)
	if err != nil {
		return 0, nil, stacktrace.Propagate(err, "An error occurred getting the containers for enclave '%v'", enclaveUuid)
	}
	if len(allEnclaveContainers) == 0 {
		return enclave.EnclaveStatus_Empty, nil, nil
	}

	resultEnclaveStatus := enclave.EnclaveStatus_Stopped
	for _, enclaveContainer := range allEnclaveContainers {
		containerStatus := enclaveContainer.GetStatus()
		isContainerRunning, found := consts.IsContainerRunningDeterminer[containerStatus]
		if !found {
			// This should never happen because we enforce completeness in a unit test
			return 0, nil, stacktrace.NewError("No is-running designation found for enclave container status '%v'; this is a bug in Kurtosis!", containerStatus.String())
		}
		if isContainerRunning {
			resultEnclaveStatus = enclave.EnclaveStatus_Running
			//Enclave is considered running if we found at least one container running
			break
		}
	}

	return resultEnclaveStatus, allEnclaveContainers, nil
}

func (backend *DockerKurtosisBackend) getAllEnclaveContainers(
	ctx context.Context,
	enclaveUuid enclave.EnclaveUUID,
) ([]*types.Container, error) {

	var containers []*types.Container

	searchLabels := map[string]string{
		label_key_consts.AppIDDockerLabelKey.GetString():       label_value_consts.AppIDDockerLabelValue.GetString(),
		label_key_consts.EnclaveUUIDDockerLabelKey.GetString(): string(enclaveUuid),
	}
	containers, err := backend.dockerManager.GetContainersByLabels(ctx, searchLabels, shouldFetchStoppedContainersWhenGettingEnclaveStatus)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting the containers for enclave '%v' by labels '%+v'", enclaveUuid, searchLabels)
	}
	return containers, nil
}

func getAllEnclaveVolumes(
	ctx context.Context,
	dockerManager *docker_manager.DockerManager,
	enclaveUuid enclave.EnclaveUUID,
) ([]*docker_types.Volume, error) {

	var volumes []*docker_types.Volume

	searchLabels := map[string]string{
		label_key_consts.AppIDDockerLabelKey.GetString():       label_value_consts.AppIDDockerLabelValue.GetString(),
		label_key_consts.EnclaveUUIDDockerLabelKey.GetString(): string(enclaveUuid),
	}

	volumes, err := dockerManager.GetVolumesByLabels(ctx, searchLabels)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred getting the volumes for enclave '%v' by labels '%+v'", enclaveUuid, searchLabels)
	}

	return volumes, nil
}

func createDumpContainerJob(
	ctx context.Context,
	dockerManager *docker_manager.DockerManager,
	enclaveOutputDirpath string,
	resultErrsChan chan error,
	containerName string,
	containerId string,
) func() {
	return func() {
		if err := dumpContainerInfo(ctx, dockerManager, enclaveOutputDirpath, containerName, containerId); err != nil {
			resultErrsChan <- stacktrace.Propagate(
				err,
				"An error occurred dumping container info for container with name '%v' and ID '%v'",
				containerName,
				containerId,
			)
		}
	}
}

func dumpContainerInfo(
	ctx context.Context,
	dockerManager *docker_manager.DockerManager,
	enclaveOutputDirpath string,
	containerName string,
	containerId string,
) error {
	// Make output directory
	containerOutputDirpath := path.Join(enclaveOutputDirpath, containerName)
	if err := os.Mkdir(containerOutputDirpath, createdDirPerms); err != nil {
		return stacktrace.Propagate(
			err,
			"An error occurred creating directory '%v' to hold the output of container with name '%v' and ID '%v'",
			containerOutputDirpath,
			containerName,
			containerId,
		)
	}

	// Write container inspect results to file
	inspectResult, err := dockerManager.InspectContainer(
		ctx,
		containerId,
	)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred inspecting container with ID '%v'", containerId)
	}
	jsonSerializedInspectResultBytes, err := json.MarshalIndent(inspectResult, containerSpecJsonSerializationPrefix, containerSpecJsonSerializationIndent)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred serializing the results of inspecting container with ID '%v' to JSON", containerId)
	}
	specOutputFilepath := path.Join(containerOutputDirpath, containerInspectResultFilename)
	if err := ioutil.WriteFile(specOutputFilepath, jsonSerializedInspectResultBytes, createdFilePerms); err != nil {
		return stacktrace.Propagate(
			err,
			"An error occurred writing the inspect output of container with name '%v' and ID '%v' to file '%v'",
			containerName,
			containerId,
			specOutputFilepath,
		)
	}

	// Write container logs to file
	containerLogsReadCloser, err := dockerManager.GetContainerLogs(ctx, containerId, shouldFollowContainerLogsWhenDumping)
	if err != nil {
		return stacktrace.Propagate(err, "An error occurred getting the logs for container with ID '%v'", containerId)
	}
	defer containerLogsReadCloser.Close()

	logsOutputFilepath := path.Join(containerOutputDirpath, containerLogsFilename)
	logsOutputFp, err := os.Create(logsOutputFilepath)
	if err != nil {
		return stacktrace.Propagate(
			err,
			"An error occurred creating file '%v' to hold the logs of container with name '%v' and ID '%v'",
			logsOutputFilepath,
			containerName,
			containerId,
		)
	}
	defer logsOutputFp.Close()

	// TODO Push this down into DockerManager as this is copied in multiple places!!! This check-if-the-container-is-TTY-and-use-io.Copy-if-so-and-stdcopy-if-not
	//  is copied straight from the Docker CLI, but it REALLY sucks that a Kurtosis dev magically needs to know that that's what
	//  they have to do if they want to read container logs
	// If we don't have this, reading the logs from a TTY container breaks
	if inspectResult.Config.Tty {
		if _, err := io.Copy(logsOutputFp, containerLogsReadCloser); err != nil {
			return stacktrace.Propagate(
				err,
				"An error occurred copying the TTY container logs stream to file '%v' for container with name '%v' and ID '%v'",
				logsOutputFilepath,
				containerName,
				containerId,
			)
		}
	} else {
		if _, err := stdcopy.StdCopy(logsOutputFp, logsOutputFp, containerLogsReadCloser); err != nil {
			return stacktrace.Propagate(
				err,
				"An error occurred copying the non-TTY container logs stream to file '%v' for container with name '%v' and ID '%v'",
				logsOutputFilepath,
				containerName,
				containerId,
			)
		}
	}

	return nil
}

func destroyContainersInEnclaves(
	ctx context.Context,
	dockerManager *docker_manager.DockerManager,
	enclaves map[enclave.EnclaveUUID]*matchingNetworkInformation,
) (
	map[enclave.EnclaveUUID]bool,
	map[enclave.EnclaveUUID]error,
	error,
) {
	// For all the enclaves to destroy, gather all the containers that should be destroyed
	enclaveUuidsForContainerIdsToRemove := map[string]enclave.EnclaveUUID{}
	containerIdsToRemove := map[string]bool{}
	for enclaveUuid, networkInfo := range enclaves {
		for _, container := range networkInfo.containers {
			containerId := container.GetId()
			enclaveUuidsForContainerIdsToRemove[containerId] = enclaveUuid
			containerIdsToRemove[containerId] = true
		}
	}

	var removeEnclaveContainerOperation docker_operation_parallelizer.DockerOperation = func(ctx context.Context, dockerManager *docker_manager.DockerManager, dockerObjectId string) error {
		if err := dockerManager.RemoveContainer(ctx, dockerObjectId); err != nil {
			return stacktrace.Propagate(err, "An error occurred removing enclave container with ID '%v'", dockerObjectId)
		}
		return nil
	}

	_, erroredContainerIds := docker_operation_parallelizer.RunDockerOperationInParallel(
		ctx,
		containerIdsToRemove,
		dockerManager,
		removeEnclaveContainerOperation,
	)

	containerRemovalErrorStrsByEnclave := map[enclave.EnclaveUUID][]string{}
	for erroredContainerId, removeContainerErr := range erroredContainerIds {
		containerEnclaveUuid, found := enclaveUuidsForContainerIdsToRemove[erroredContainerId]
		if !found {
			return nil, nil, stacktrace.NewError("An error occurred destroying container '%v' in an enclave we didn't request", erroredContainerId)
		}

		existingEnclaveErrors, found := containerRemovalErrorStrsByEnclave[containerEnclaveUuid]
		if !found {
			existingEnclaveErrors = []string{}
		}
		containerRemovalErrorStrsByEnclave[containerEnclaveUuid] = append(existingEnclaveErrors, removeContainerErr.Error())
	}

	erroredEnclaveUuids := map[enclave.EnclaveUUID]error{}
	successfulEnclaveUuids := map[enclave.EnclaveUUID]bool{}
	for enclaveUuid := range enclaves {
		containerRemovalErrorStrs, found := containerRemovalErrorStrsByEnclave[enclaveUuid]
		if !found || len(containerRemovalErrorStrs) == 0 {
			successfulEnclaveUuids[enclaveUuid] = true
			continue
		}

		errorStr := strings.Join(containerRemovalErrorStrs, "\n\n")
		erroredEnclaveUuids[enclaveUuid] = stacktrace.NewError(
			"One or more errors occurred removing the containers in enclave '%v':\n%v",
			enclaveUuid,
			errorStr,
		)
	}

	return successfulEnclaveUuids, erroredEnclaveUuids, nil
}

func destroyVolumesInEnclaves(
	ctx context.Context,
	dockerManager *docker_manager.DockerManager,
	enclaves map[enclave.EnclaveUUID]bool,
) (
	map[enclave.EnclaveUUID]bool,
	map[enclave.EnclaveUUID]error,
	error,
) {
	// After we've tried to destroy all the containers from the enclaves, take the successful ones and destroy their volumes
	enclaveUuidsForVolumeIdsToRemove := map[string]enclave.EnclaveUUID{}
	volumeIdsToRemove := map[string]bool{}
	for enclaveUuid := range enclaves {
		enclaveVolumeIds, err := getAllEnclaveVolumes(ctx, dockerManager, enclaveUuid)
		if err != nil {
			return nil, nil, stacktrace.Propagate(err, "An error occurred getting the volumes for enclave '%v'", enclaveUuid)
		}

		for _, volume := range enclaveVolumeIds {
			volumeId := volume.Name
			enclaveUuidsForVolumeIdsToRemove[volumeId] = enclaveUuid
			volumeIdsToRemove[volumeId] = true
		}
	}

	var removeEnclaveVolumeOperation docker_operation_parallelizer.DockerOperation = func(ctx context.Context, dockerManager *docker_manager.DockerManager, dockerObjectId string) error {
		if err := dockerManager.RemoveVolume(ctx, dockerObjectId); err != nil {
			return stacktrace.Propagate(err, "An error occurred removing enclave volume with ID '%v'", dockerObjectId)
		}
		return nil
	}

	_, erroredVolumeIds := docker_operation_parallelizer.RunDockerOperationInParallel(
		ctx,
		volumeIdsToRemove,
		dockerManager,
		removeEnclaveVolumeOperation,
	)

	volumeRemovalErrorStrsByEnclave := map[enclave.EnclaveUUID][]string{}
	for erroredVolumeId, removeVolumeErr := range erroredVolumeIds {
		volumeEnclaveUuid, found := enclaveUuidsForVolumeIdsToRemove[erroredVolumeId]
		if !found {
			return nil, nil, stacktrace.NewError("An error occurred removing volume '%v' in an enclave we didn't request", erroredVolumeId)
		}

		existingEnclaveErrors, found := volumeRemovalErrorStrsByEnclave[volumeEnclaveUuid]
		if !found {
			existingEnclaveErrors = []string{}
		}
		volumeRemovalErrorStrsByEnclave[volumeEnclaveUuid] = append(existingEnclaveErrors, removeVolumeErr.Error())
	}

	erroredEnclaveUuids := map[enclave.EnclaveUUID]error{}
	successfulEnclaveUuids := map[enclave.EnclaveUUID]bool{}
	for enclaveUuid := range enclaves {
		containerRemovalErrorStrs, found := volumeRemovalErrorStrsByEnclave[enclaveUuid]
		if !found || len(containerRemovalErrorStrs) == 0 {
			successfulEnclaveUuids[enclaveUuid] = true
			continue
		}

		errorStr := strings.Join(containerRemovalErrorStrs, "\n\n")
		erroredEnclaveUuids[enclaveUuid] = stacktrace.NewError(
			"One or more errors occurred removing the volumes in enclave '%v':\n%v",
			enclaveUuid,
			errorStr,
		)
	}

	return successfulEnclaveUuids, erroredEnclaveUuids, nil
}

func destroyEnclaveNetworks(
	ctx context.Context,
	dockerManager *docker_manager.DockerManager,
	enclaveNetworkIds map[enclave.EnclaveUUID]string,
) (
	map[enclave.EnclaveUUID]bool,
	map[enclave.EnclaveUUID]error,
	error,
) {
	networkIdsToRemove := map[string]bool{}
	enclaveUuidsForNetworkIds := map[string]enclave.EnclaveUUID{}
	for enclaveUuid, networkId := range enclaveNetworkIds {
		networkIdsToRemove[networkId] = true
		enclaveUuidsForNetworkIds[networkId] = enclaveUuid
	}

	var removeNetworkOperation docker_operation_parallelizer.DockerOperation = func(ctx context.Context, dockerManager *docker_manager.DockerManager, dockerObjectId string) error {
		if err := dockerManager.RemoveNetwork(ctx, dockerObjectId); err != nil {
			return stacktrace.Propagate(err, "An error occurred removing enclave network with ID '%v'", dockerObjectId)
		}
		return nil
	}

	successfulNetworkIds, erroredNetworkIds := docker_operation_parallelizer.RunDockerOperationInParallel(
		ctx,
		networkIdsToRemove,
		dockerManager,
		removeNetworkOperation,
	)

	successfulEnclaveUuids := map[enclave.EnclaveUUID]bool{}
	for networkId := range successfulNetworkIds {
		enclaveUuid, found := enclaveUuidsForNetworkIds[networkId]
		if !found {
			return nil, nil, stacktrace.NewError("Docker network '%v' was successfully deleted, but wasn't requested to be deleted", networkId)
		}
		successfulEnclaveUuids[enclaveUuid] = true
	}

	erroredEnclaveUuids := map[enclave.EnclaveUUID]error{}
	for networkId, networkRemovalErr := range erroredNetworkIds {
		enclaveUuid, found := enclaveUuidsForNetworkIds[networkId]
		if !found {
			return nil, nil, stacktrace.NewError("Docker network '%v' had the following error during deletion, but wasn't requested to be deleted:\n%v", networkId, networkRemovalErr)
		}
		erroredEnclaveUuids[enclaveUuid] = networkRemovalErr
	}

	return successfulEnclaveUuids, erroredEnclaveUuids, nil
}

func getEnclaveUuidFromNetwork(network *types.Network) (enclave.EnclaveUUID, error) {
	labels := network.GetLabels()
	enclaveUuidLabelValue, found := labels[label_key_consts.EnclaveUUIDDockerLabelKey.GetString()]
	if !found {
		return "", stacktrace.NewError("Expected to find network's label with key '%v' but none was found", label_key_consts.EnclaveUUIDDockerLabelKey.GetString())
	}
	enclaveUuid := enclave.EnclaveUUID(enclaveUuidLabelValue)
	return enclaveUuid, nil
}

func getEnclaveCreationTimeFromNetwork(network *types.Network) (*time.Time, error) {

	labels := network.GetLabels()
	enclaveCreationTimeStr, found := labels[label_key_consts.EnclaveCreationTimeLabelKey.GetString()]
	if !found {
		//Handling retro-compatibility, enclaves that did not track enclave's creation time
		return nil, nil //TODO remove this return after 2023-01-01
		//TODO uncomment this after 2023-01-01 when we are sure that there is not any old enclave created with the creation time annotation
		//return nil, stacktrace.NewError("Expected to find network's label with key '%v' but none was found", label_key_consts.EnclaveCreationTimeLabelKey.GetString())
	}

	enclaveCreationTime, err := time.Parse(time.RFC3339, enclaveCreationTimeStr)
	if err != nil {
		return nil, stacktrace.Propagate(err, "An error occurred parsing enclave creation time '%v' using this format '%v'", enclaveCreationTimeStr, time.RFC3339)
	}

	return &enclaveCreationTime, nil
}

func getEnclaveNameFromNetwork(network *types.Network) string {

	labels := network.GetLabels()
	enclaveNameStr, found := labels[label_key_consts.EnclaveNameDockerLabelKey.GetString()]
	if !found {
		//Handling retro-compatibility, enclaves that did not track enclave's name
		return ""
	}

	return enclaveNameStr
}
