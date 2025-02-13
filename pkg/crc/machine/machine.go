package machine

import (
	"encoding/json"
	"fmt"
	"github.com/code-ready/crc/pkg/crc/systemd"
	"github.com/code-ready/crc/pkg/os"
	"io/ioutil"
	"path/filepath"
	"time"

	"github.com/code-ready/crc/pkg/crc/constants"
	"github.com/code-ready/crc/pkg/crc/errors"
	"github.com/code-ready/crc/pkg/crc/logging"

	// host and instance related
	"github.com/code-ready/crc/pkg/crc/network"
	// cluster services
	"github.com/code-ready/crc/pkg/crc/services"
	"github.com/code-ready/crc/pkg/crc/services/dns"

	// machine related imports
	"github.com/code-ready/crc/pkg/crc/machine/bundle"
	"github.com/code-ready/crc/pkg/crc/machine/config"
	"github.com/code-ready/crc/pkg/crc/machine/libvirt"
	"github.com/code-ready/crc/pkg/crc/machine/virtualbox"

	"github.com/code-ready/machine/libmachine"
	"github.com/code-ready/machine/libmachine/host"
	"github.com/code-ready/machine/libmachine/log"
	"github.com/code-ready/machine/libmachine/state"
)

func Start(startConfig StartConfig) (StartResult, error) {
	defer unsetMachineLogging()

	result := &StartResult{Name: startConfig.Name}

	// Set libmachine logging
	err := setMachineLogging(startConfig.Debug)
	if err != nil {
		return *result, err
	}

	libMachineAPIClient := libmachine.NewClient(constants.MachineBaseDir, constants.MachineCertsDir)
	defer libMachineAPIClient.Close()

	machineConfig := config.MachineConfig{
		Name:       startConfig.Name,
		BundlePath: startConfig.BundlePath,
		VMDriver:   startConfig.VMDriver,
		CPUs:       startConfig.CPUs,
		Memory:     startConfig.Memory,
	}

	logging.InfoF("Extracting the Bundle tarball ...")
	crcBundleMetadata, extractedPath, err := bundle.GetCrcBundleInfo(machineConfig)
	if err != nil {
		logging.ErrorF("Error to get bundle Metadata %v", err)
		result.Error = err.Error()
		return *result, err
	}

	// Retrieve metadata info
	diskPath := filepath.Join(extractedPath, crcBundleMetadata.Storage.DiskImages[0].Name)
	machineConfig.DiskPathURL = fmt.Sprintf("file://%s", diskPath)
	machineConfig.SSHKeyPath = filepath.Join(extractedPath, crcBundleMetadata.ClusterInfo.SSHPrivateKeyFile)

	// Get the content of kubeadmin-password file
	kubeadminPassword, err := ioutil.ReadFile(filepath.Join(extractedPath, crcBundleMetadata.ClusterInfo.KubeadminPasswordFile))
	if err != nil {
		logging.ErrorF("Error reading the %s file %v", filepath.Join(extractedPath, crcBundleMetadata.ClusterInfo.KubeadminPasswordFile), err)
		result.Error = err.Error()
		return *result, err
	}

	// Put ClusterInfo to StartResult config.
	clusterConfig := ClusterConfig{
		KubeConfig:    filepath.Join(extractedPath, crcBundleMetadata.ClusterInfo.KubeConfig),
		KubeAdminPass: string(kubeadminPassword),
		WebConsoleURL: constants.DefaultWebConsoleURL,
		ClusterAPI:    constants.DefaultAPIURL,
	}

	result.ClusterConfig = clusterConfig

	// Pre-VM start
	driverInfo, _ := getDriverInfo(startConfig.VMDriver)
	exists, err := existVM(libMachineAPIClient, machineConfig)
	if !exists {
		logging.InfoF("Creating VM ...")

		host, err := createHost(libMachineAPIClient, machineConfig)
		if err != nil {
			logging.ErrorF("Error creating host: %v", err)
			result.Error = err.Error()
		}

		vmState, err := host.Driver.GetState()
		if err != nil {
			logging.ErrorF("Error getting the state for host: %v", err)
			result.Error = err.Error()
		}

		result.Status = vmState.String()
	} else {
		host, err := libMachineAPIClient.Load(machineConfig.Name)
		s, err := host.Driver.GetState()
		if err != nil {
			logging.ErrorF("Error getting the state for host: %v", err)
			result.Error = err.Error()
		}
		if s == state.Running {
			result.Status = s.String()
			return *result, nil
		}

		if s != state.Running {
			logging.InfoF("Starting stopped VM ...")
			if err := host.Driver.Start(); err != nil {
				logging.ErrorF("Error starting stopped VM: %v", err)
				result.Error = err.Error()
			}
			if err := libMachineAPIClient.Save(host); err != nil {
				logging.ErrorF("Error saving state for VM: %v", err)
				result.Error = err.Error()
			}
		}

		vmState, err := host.Driver.GetState()
		if err != nil {
			logging.ErrorF("Error getting the state: %v", err)
			result.Error = err.Error()
		}

		result.Status = vmState.String()
	}

	// Post-VM start
	host, err := libMachineAPIClient.Load(machineConfig.Name)
	instanceIP, err := host.Driver.GetIP()
	if err != nil {
		logging.ErrorF("Error getting the IP: %v", err)
		result.Error = err.Error()
		return *result, err
	}

	hostIP, err := network.DetermineHostIP(instanceIP)
	if err != nil {
		logging.ErrorF("Error determining host IP: %v", err)
		result.Error = err.Error()
		return *result, err
	}
	logging.InfoF("Bridge IP on the host: %s", hostIP)

	// Create servicePostStartConfig for dns checks and dns start.
	servicePostStartConfig := services.ServicePostStartConfig{
		Name: startConfig.Name,
		// TODO: would prefer passing in a more generic type
		Driver: host.Driver,
		IP:     instanceIP,
		HostIP: hostIP,
		// TODO: should be more finegrained
		BundleMetadata: *crcBundleMetadata,
	}

	// If driver need dns service then start it
	if driverInfo.UseDNSService {
		if _, err := dns.RunPostStart(servicePostStartConfig); err != nil {
			logging.ErrorF("Error running post start: %v", err)
			result.Error = err.Error()
			return *result, err
		}
	}
	// Check DNS looksup before starting the kubelet
	if queryOutput, err := dns.CheckCRCLocalDNSReachable(servicePostStartConfig); err != nil {
		logging.ErrorF("Failed internal dns query: %v : %s", err, queryOutput)
		result.Error = err.Error()
		return *result, err
	}
	logging.InfoF("Check internal and public dns query ...")

	if queryOutput, err := dns.CheckCRCPublicDNSReachable(servicePostStartConfig); err != nil {
		logging.WarnF("Failed Public dns query: %v : %s", err, queryOutput)
	}

	// Start kubelet inside the VM
	sd := systemd.NewInstanceSystemdCommander(host.Driver)
	kubeletStarted, err := sd.Start("kubelet")
	if err != nil {
		logging.ErrorF("Error starting kubelet: %s", err)
		result.Error = err.Error()
	}
	if kubeletStarted {
		logging.InfoF("Starting OpenShift cluster ... [waiting 3m]")
	}
	result.KubeletStarted = kubeletStarted
	//

	// If no error, return usage message
	if result.Error == "" {
		time.Sleep(time.Minute * 3)
		logging.InfoF("To access the cluster using 'oc', run 'oc login -u kubeadmin -p %s %s'", result.ClusterConfig.KubeAdminPass, result.ClusterConfig.ClusterAPI)
		logging.InfoF("Access the OpenShift web-console here: %s", result.ClusterConfig.WebConsoleURL)
		logging.InfoF("Login to the console with user: kubeadmin, password: %s", result.ClusterConfig.KubeAdminPass)
		if os.CurrentOS() == os.DARWIN {
			logging.WarnF(fmt.Sprintf("Make sure to add 'nameserver %s' as first entry to '/etc/resolv.conf' file", instanceIP))
		}
	}

	return *result, err
}

func Stop(stopConfig StopConfig) (StopResult, error) {
	defer unsetMachineLogging()

	result := &StopResult{Name: stopConfig.Name}
	// Set libmachine logging
	err := setMachineLogging(stopConfig.Debug)
	if err != nil {
		return *result, err
	}

	libMachineAPIClient := libmachine.NewClient(constants.MachineBaseDir, constants.MachineCertsDir)
	host, err := libMachineAPIClient.Load(stopConfig.Name)

	if err != nil {
		result.Success = false
		result.Error = err.Error()
		return *result, err
	}

	result.State, _ = host.Driver.GetState()

	if err := host.Stop(); err != nil {
		result.Success = false
		result.Error = err.Error()
		return *result, err
	}

	result.Success = true
	return *result, nil
}

func PowerOff(PowerOff PowerOffConfig) (PowerOffResult, error) {
	result := &PowerOffResult{Name: PowerOff.Name}

	libMachineAPIClient := libmachine.NewClient(constants.MachineBaseDir, constants.MachineCertsDir)
	host, err := libMachineAPIClient.Load(PowerOff.Name)

	if err != nil {
		result.Success = false
		result.Error = err.Error()
		return *result, err
	}

	if err := host.Kill(); err != nil {
		result.Success = false
		result.Error = err.Error()
		return *result, err
	}

	result.Success = true
	return *result, nil
}

func Delete(deleteConfig DeleteConfig) (DeleteResult, error) {
	result := &DeleteResult{Name: deleteConfig.Name, Success: true}

	libMachineAPIClient := libmachine.NewClient(constants.MachineBaseDir, constants.MachineCertsDir)
	host, err := libMachineAPIClient.Load(deleteConfig.Name)

	if err != nil {
		result.Success = false
		result.Error = err.Error()
		return *result, err
	}

	m := errors.MultiError{}
	m.Collect(host.Driver.Remove())
	m.Collect(libMachineAPIClient.Remove(deleteConfig.Name))

	if len(m.Errors) != 0 {
		result.Success = false
		result.Error = m.ToError().Error()
		return *result, m.ToError()
	}
	return *result, nil
}

func existVM(api libmachine.API, machineConfig config.MachineConfig) (bool, error) {
	exists, err := api.Exists(machineConfig.Name)
	if err != nil {
		return false, errors.NewF("Error checking if the host exists: %s", err)
	}
	return exists, nil
}

func createHost(api libmachine.API, machineConfig config.MachineConfig) (*host.Host, error) {
	driverOptions := getDriverOptions(machineConfig)
	jsonDriverConfig, err := json.Marshal(driverOptions)

	vm, err := api.NewHost(machineConfig.VMDriver, jsonDriverConfig)

	if err != nil {
		return nil, errors.NewF("Error creating new host: %s", err)
	}

	if err := api.Create(vm); err != nil {
		return nil, errors.NewF("Error creating the VM. %s", err)
	}

	return vm, nil
}

func getDriverOptions(machineConfig config.MachineConfig) interface{} {
	var driver interface{}

	// Supported drivers
	switch machineConfig.VMDriver {

	case "libvirt":
		driver = libvirt.CreateHost(machineConfig)
	case "virtualbox":
		driver = virtualbox.CreateHost(machineConfig)

	default:
		errors.ExitWithMessage(1, "Unsupported driver: %s", machineConfig.VMDriver)
	}

	return driver
}

func setMachineLogging(logs bool) error {
	if !logs {
		log.SetDebug(true)
		logging.RemoveFileHook()
		logfile, err := logging.OpenLogFile()
		if err != nil {
			return err
		}
		log.SetOutWriter(logfile)
		log.SetErrWriter(logfile)
	} else {
		log.SetDebug(true)
	}
	return nil
}

func unsetMachineLogging() {
	logging.CloseLogFile()
	logging.SetupFileHook()
}
