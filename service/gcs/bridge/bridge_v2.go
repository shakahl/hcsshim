package bridge

import (
	"encoding/json"
	"syscall"
	"time"

	"github.com/Microsoft/opengcs/internal/runtime/hcsv2"
	"github.com/Microsoft/opengcs/service/gcs/gcserr"
	"github.com/Microsoft/opengcs/service/gcs/prot"
	"github.com/Microsoft/opengcs/service/gcs/stdio"
	"github.com/Microsoft/opengcs/service/libs/commonutils"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// The capabilities of this GCS.
var capabilities = prot.GcsCapabilities{
	SendHostCreateMessage:   false,
	SendHostStartMessage:    false,
	HVSocketConfigOnStartup: false,
	SupportedSchemaVersions: []prot.SchemaVersion{
		{
			Major: 2,
			Minor: 1,
		},
	},
	RuntimeOsType: prot.OsTypeLinux,
	GuestDefinedCapabilities: prot.GcsGuestCapabilities{
		NamespaceAddRequestSupported: true,
		SignalProcessSupported:       true,
	},
}

// negotiateProtocolV2 was introduced in v4 so will not be called with a minimum
// lower than that.
func (b *Bridge) negotiateProtocolV2(r *Request) (RequestResponse, error) {
	var request prot.NegotiateProtocol
	if err := commonutils.UnmarshalJSONWithHresult(r.Message, &request); err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal JSON in message \"%s\"", r.Message)
	}

	logrus.WithFields(logrus.Fields{
		"activityID":      request.ActivityID,
		"cid":             request.ContainerID,
		"minimum-version": request.MinimumVersion,
		"maximum-version": request.MaximumVersion,
	}).Info("opengcs::bridge::negotiateProtocolV2")

	if request.MaximumVersion < uint32(prot.PvV4) || uint32(prot.PvMax) < request.MinimumVersion {
		return nil, gcserr.NewHresultError(gcserr.HrVmcomputeUnsupportedProtocolVersion)
	}

	min := func(x, y uint32) uint32 {
		if x < y {
			return x
		}
		return y
	}

	major := min(uint32(prot.PvMax), request.MaximumVersion)

	// Set our protocol selected version before return.
	b.protVer = prot.ProtocolVersion(major)

	return &prot.NegotiateProtocolResponse{
		Version:      major,
		Capabilities: capabilities,
	}, nil
}

// createContainerV2 creates a container based on the settings passed in `r`.
//
// This is allowed only for protocol version 4+, schema version 2.1+
func (b *Bridge) createContainerV2(r *Request) (RequestResponse, error) {
	var request prot.ContainerCreate
	if err := commonutils.UnmarshalJSONWithHresult(r.Message, &request); err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal JSON in message \"%s\"", r.Message)
	}

	logrus.WithFields(logrus.Fields{
		"activityID": request.ActivityID,
		"cid":        request.ContainerID,
	}).Info("opengcs::bridge::createContainerV2")

	var settingsV2 prot.VMHostedContainerSettingsV2
	if err := commonutils.UnmarshalJSONWithHresult([]byte(request.ContainerConfig), &settingsV2); err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal JSON for ContainerConfig \"%s\"", request.ContainerConfig)
	}

	if settingsV2.SchemaVersion.Cmp(prot.SchemaVersion{Major: 2, Minor: 1}) < 0 {
		return nil, gcserr.WrapHresult(
			errors.Errorf("invalid schema version: %v", settingsV2.SchemaVersion),
			gcserr.HrVmcomputeInvalidJSON)
	}

	c, err := b.hostState.CreateContainer(request.ContainerID, &settingsV2)
	if err != nil {
		return nil, err
	}
	waitFn := func() prot.NotificationType {
		return c.Wait()
	}

	go func() {
		nt := waitFn()
		notification := &prot.ContainerNotification{
			MessageBase: prot.MessageBase{
				ContainerID: request.ContainerID,
				ActivityID:  request.ActivityID,
			},
			Type:       nt,
			Operation:  prot.AoNone,
			Result:     0,
			ResultInfo: "",
		}
		b.PublishNotification(notification)
	}()

	return &prot.ContainerCreateResponse{}, nil
}

// startContainerV2 doesn't have a great correlation to LCOW. On Windows this is
// used to start the container silo. In Linux the container is the process so we
// wait until the exec process of the init process to actually issue the start.
//
// This is allowed only for protocol version 4+, schema version 2.1+
func (b *Bridge) startContainerV2(r *Request) (RequestResponse, error) {
	// This is just a noop, but needs to be handled so that an error isn't
	// returned to the HCS.
	var request prot.MessageBase
	if err := commonutils.UnmarshalJSONWithHresult(r.Message, &request); err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal JSON in message \"%s\"", r.Message)
	}

	logrus.WithFields(logrus.Fields{
		"activityID": request.ActivityID,
		"cid":        request.ContainerID,
	}).Info("opengcs::bridge::startContainerV2")

	return &prot.MessageResponseBase{}, nil
}

// execProcessV2 is used to execute three types of processes in the guest.
//
// 1. HostProcess. This is a process in the Host pid namespace that runs as
// root. It is signified by either `request.IsExternal` or `request.ContainerID
// == hcsv2.UVMContainerID`.
//
// 2. Container Init process. This is the init process of the created container.
// We use exec for this instead of `StartContainer` because the protocol does
// not pass in the appropriate std pipes for relaying the results until exec.
// Until this is called the container remains in the `created` state.
//
// 3. Container Exec process. This is a process that is run in the container's
// pid namespace.
//
// This is allowed only for protocol version 4+, schema version 2.1+
func (b *Bridge) execProcessV2(r *Request) (RequestResponse, error) {
	var request prot.ContainerExecuteProcess
	if err := commonutils.UnmarshalJSONWithHresult(r.Message, &request); err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal JSON in message \"%s\"", r.Message)
	}

	logrus.WithFields(logrus.Fields{
		"activityID": request.ActivityID,
		"cid":        request.ContainerID,
	}).Info("opengcs::bridge::execProcessV2")

	// The request contains a JSON string field which is equivalent to an
	// ExecuteProcessInfo struct.
	var params prot.ProcessParameters
	if err := commonutils.UnmarshalJSONWithHresult([]byte(request.Settings.ProcessParameters), &params); err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal JSON for ProcessParameters \"%s\"", request.Settings.ProcessParameters)
	}

	var conSettings stdio.ConnectionSettings
	if params.CreateStdInPipe {
		conSettings.StdIn = &request.Settings.VsockStdioRelaySettings.StdIn
	}
	if params.CreateStdOutPipe {
		conSettings.StdOut = &request.Settings.VsockStdioRelaySettings.StdOut
	}
	if params.CreateStdErrPipe {
		conSettings.StdErr = &request.Settings.VsockStdioRelaySettings.StdErr
	}

	var pid int
	var c *hcsv2.Container
	var err error
	if params.IsExternal || request.ContainerID == hcsv2.UVMContainerID {
		pid, err = b.coreint.RunExternalProcess(params, conSettings)
	} else if c, err = b.hostState.GetContainer(request.ContainerID); err == nil {
		// We found a V2 container. Treat this as a V2 process.
		if params.OCIProcess == nil {
			pid, err = c.Start(conSettings)
		} else {
			pid, err = c.ExecProcess(params.OCIProcess, conSettings)
		}
	}

	if err != nil {
		return nil, err
	}

	return &prot.ContainerExecuteProcessResponse{
		ProcessID: uint32(pid),
	}, nil
}

// killContainerV2 is a user forced terminate of the container and all processes
// in the container. It is equivalent to sending SIGKILL to the init process and
// all exec'd processes.
//
// This is allowed only for protocol version 4+, schema version 2.1+
func (b *Bridge) killContainerV2(r *Request) (RequestResponse, error) {
	return b.signalContainer(r, unix.SIGKILL)
}

// shutdownContainerV2 is a user requested shutdown of the container and all
// processes in the container. It is equivalent to sending SIGTERM to the init
// process and all exec'd processes.
//
// This is allowed only for protocol version 4+, schema version 2.1+
func (b *Bridge) shutdownContainerV2(r *Request) (RequestResponse, error) {
	return b.signalContainer(r, unix.SIGTERM)
}

// signalContainerV2 is not a handler func. This is because the actual signal is
// implied based on the message type of either `killContainerV2` or
// `shutdownContainerV2`.
func (b *Bridge) signalContainerV2(r *Request, signal syscall.Signal) (RequestResponse, error) {
	var request prot.MessageBase
	if err := commonutils.UnmarshalJSONWithHresult(r.Message, &request); err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal JSON in message \"%s\"", r.Message)
	}

	log := logrus.WithFields(logrus.Fields{
		"activityID": request.ActivityID,
		"cid":        request.ContainerID,
		"signal":     signal,
	})
	log.Info("opengcs::bridge::signalContainerV2")

	// If this is targeting the UVM send the request to the host itself.
	if request.ContainerID == hcsv2.UVMContainerID {
		// We are asking to shutdown the UVM itself.
		if signal != unix.SIGTERM {
			log.Error("opengcs::bridge::signalContainerV2 - invalid signal for uvm")
		}
		// This is a destructive call. We do not respond to the HCS
		b.quitChan <- true
		b.hostState.Shutdown()
	} else {
		c, err := b.hostState.GetContainer(request.ContainerID)
		if err != nil {
			return nil, err
		}

		err = c.Kill(signal)
		if err != nil {
			return nil, err
		}
	}

	return &prot.MessageResponseBase{}, nil
}

func (b *Bridge) signalProcessV2(r *Request) (RequestResponse, error) {
	var request prot.ContainerSignalProcess
	if err := commonutils.UnmarshalJSONWithHresult(r.Message, &request); err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal JSON in message \"%s\"", r.Message)
	}

	logrus.WithFields(logrus.Fields{
		"activityID": request.ActivityID,
		"cid":        request.ContainerID,
		"pid":        request.ProcessID,
		"signal":     request.Options.Signal,
	}).Info("opengcs::bridge::signalProcessV2")

	c, err := b.hostState.GetContainer(request.ContainerID)
	if err != nil {
		return nil, err
	}

	p, err := c.GetProcess(request.ProcessID)
	if err != nil {
		return nil, err
	}

	var signal syscall.Signal
	if request.Options.Signal == 0 {
		signal = unix.SIGKILL
	} else {
		signal = syscall.Signal(request.Options.Signal)
	}
	if err := p.Kill(signal); err != nil {
		return nil, err
	}

	return &prot.MessageResponseBase{}, nil
}

func (b *Bridge) getPropertiesV2(r *Request) (RequestResponse, error) {
	var request prot.ContainerGetProperties
	if err := commonutils.UnmarshalJSONWithHresult(r.Message, &request); err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal JSON in message \"%s\"", r.Message)
	}

	logrus.WithFields(logrus.Fields{
		"activityID": request.ActivityID,
		"cid":        request.ContainerID,
	}).Info("opengcs::bridge::getPropertiesV2")

	var properties *prot.Properties
	if request.ContainerID == hcsv2.UVMContainerID {
		return nil, errors.New("getPropertiesV2 is not supported against the UVM")
	}
	c, err := b.hostState.GetContainer(request.ContainerID)
	if err != nil {
		return nil, err
	}

	pids, err := c.GetAllProcessPids()
	if err != nil {
		return nil, err
	}
	properties = &prot.Properties{
		ProcessList: make([]prot.ProcessDetails, len(pids)),
	}
	for i, pid := range pids {
		properties.ProcessList[i].ProcessID = uint32(pid)
	}

	propertyJSON := []byte("{}")
	if properties != nil {
		var err error
		propertyJSON, err = json.Marshal(properties)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to unmarshal JSON in message \"%+v\"", properties)
		}
	}

	return &prot.ContainerGetPropertiesResponse{
		Properties: string(propertyJSON),
	}, nil
}

func (b *Bridge) waitOnProcessV2(r *Request) (RequestResponse, error) {
	var request prot.ContainerWaitForProcess
	if err := commonutils.UnmarshalJSONWithHresult(r.Message, &request); err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal JSON in message \"%s\"", r.Message)
	}

	logrus.WithFields(logrus.Fields{
		"activityID": request.ActivityID,
		"cid":        request.ContainerID,
		"pid":        request.ProcessID,
		"timeout-ms": request.TimeoutInMs,
	}).Info("opengcs::bridge::waitOnProcessV2")

	var exitCodeChan <-chan int
	var doneChan chan<- bool

	// TODO: JTERRY75 - Move to hostState.ExecExternalProcess so we dont have a
	// dependency on gcscore.
	if request.ContainerID == hcsv2.UVMContainerID {
		// Pull the process from gcsCore
		var err error
		exitCodeChan, doneChan, err = b.coreint.WaitProcess(int(request.ProcessID))
		if err != nil {
			return nil, err
		}
	} else {
		c, err := b.hostState.GetContainer(request.ContainerID)
		if err != nil {
			return nil, err
		}
		p, err := c.GetProcess(request.ProcessID)
		if err != nil {
			return nil, err
		}
		exitCodeChan, doneChan = p.Wait()
	}

	// If we timed out or if we got the exit code. Acknowledge we no longer want to wait.
	defer close(doneChan)

	select {
	case exitCode := <-exitCodeChan:
		return &prot.ContainerWaitForProcessResponse{
			ExitCode: uint32(exitCode),
		}, nil
	case <-time.After(time.Duration(request.TimeoutInMs) * time.Millisecond):
		return nil, gcserr.NewHresultError(gcserr.HvVmcomputeTimeout)
	}
}

func (b *Bridge) resizeConsoleV2(r *Request) (RequestResponse, error) {
	var request prot.ContainerResizeConsole
	if err := commonutils.UnmarshalJSONWithHresult(r.Message, &request); err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal JSON in message \"%s\"", r.Message)
	}

	logrus.WithFields(logrus.Fields{
		"activityID": request.ActivityID,
		"cid":        request.ContainerID,
		"pid":        request.ProcessID,
		"height":     request.Height,
		"width":      request.Width,
	}).Info("opengcs::bridge::resizeConsoleV2")

	c, err := b.hostState.GetContainer(request.ContainerID)
	if err != nil {
		return nil, err
	}

	p, err := c.GetProcess(request.ProcessID)
	if err != nil {
		return nil, err
	}

	err = p.ResizeConsole(request.Height, request.Width)
	if err != nil {
		return nil, err
	}

	return &prot.MessageResponseBase{}, nil
}

func (b *Bridge) modifySettingsV2(r *Request) (RequestResponse, error) {
	request, err := prot.UnmarshalContainerModifySettings(r.Message)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal JSON in message \"%s\"", r.Message)
	}

	logrus.WithFields(logrus.Fields{
		"activityID": request.ActivityID,
		"cid":        request.ContainerID,
	}).Info("opengcs::bridge::modifySettingsV2")

	if request.ContainerID != hcsv2.UVMContainerID {
		return nil, errors.New("V2 Modify request not supported on anything but UVM")
	}

	err = b.hostState.ModifyHostSettings(request.Request.(*prot.ModifySettingRequest))
	if err != nil {
		return nil, err
	}

	return &prot.MessageResponseBase{}, nil
}
