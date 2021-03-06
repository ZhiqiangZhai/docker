package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/Sirupsen/logrus"
	flag "github.com/docker/docker/pkg/mflag"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/debug"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

var (
	flServiceName       = flag.String([]string{"-service-name"}, "docker", "Set the Windows service name")
	flRegisterService   = flag.Bool([]string{"-register-service"}, false, "Register the service and exit")
	flUnregisterService = flag.Bool([]string{"-unregister-service"}, false, "Unregister the service and exit")
	flRunService        = flag.Bool([]string{"-run-service"}, false, "")

	setStdHandle = syscall.NewLazyDLL("kernel32.dll").NewProc("SetStdHandle")
	oldStderr    syscall.Handle
	panicFile    *os.File

	service *handler
)

const (
	// These should match the values in event_messages.mc.
	eventInfo  = 1
	eventWarn  = 1
	eventError = 1
	eventDebug = 2
	eventPanic = 3
	eventFatal = 4

	eventExtraOffset = 10 // Add this to any event to get a string that supports extended data
)

type handler struct {
	tosvc   chan bool
	fromsvc chan error
}

type etwHook struct {
	log *eventlog.Log
}

func (h *etwHook) Levels() []logrus.Level {
	return []logrus.Level{
		logrus.PanicLevel,
		logrus.FatalLevel,
		logrus.ErrorLevel,
		logrus.WarnLevel,
		logrus.InfoLevel,
		logrus.DebugLevel,
	}
}

func (h *etwHook) Fire(e *logrus.Entry) error {
	var (
		etype uint16
		eid   uint32
	)

	switch e.Level {
	case logrus.PanicLevel:
		etype = windows.EVENTLOG_ERROR_TYPE
		eid = eventPanic
	case logrus.FatalLevel:
		etype = windows.EVENTLOG_ERROR_TYPE
		eid = eventFatal
	case logrus.ErrorLevel:
		etype = windows.EVENTLOG_ERROR_TYPE
		eid = eventError
	case logrus.WarnLevel:
		etype = windows.EVENTLOG_WARNING_TYPE
		eid = eventWarn
	case logrus.InfoLevel:
		etype = windows.EVENTLOG_INFORMATION_TYPE
		eid = eventInfo
	case logrus.DebugLevel:
		etype = windows.EVENTLOG_INFORMATION_TYPE
		eid = eventDebug
	default:
		return errors.New("unknown level")
	}

	// If there is additional data, include it as a second string.
	exts := ""
	if len(e.Data) > 0 {
		fs := bytes.Buffer{}
		for k, v := range e.Data {
			fs.WriteString(k)
			fs.WriteByte('=')
			fmt.Fprint(&fs, v)
			fs.WriteByte(' ')
		}

		exts = fs.String()[:fs.Len()-1]
		eid += eventExtraOffset
	}

	if h.log == nil {
		fmt.Fprintf(os.Stderr, "%s [%s]\n", e.Message, exts)
		return nil
	}

	var (
		ss  [2]*uint16
		err error
	)

	ss[0], err = syscall.UTF16PtrFromString(e.Message)
	if err != nil {
		return err
	}

	count := uint16(1)
	if exts != "" {
		ss[1], err = syscall.UTF16PtrFromString(exts)
		if err != nil {
			return err
		}

		count++
	}

	return windows.ReportEvent(h.log.Handle, etype, 0, eid, 0, count, 0, &ss[0], nil)
}

func getServicePath() (string, error) {
	p, err := exec.LookPath(os.Args[0])
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

func registerService() error {
	p, err := getServicePath()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	c := mgr.Config{
		ServiceType:  windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
		DisplayName:  "Docker Engine",
	}

	// Configure the service to launch with the arguments that were just passed.
	args := []string{"--run-service"}
	for _, a := range os.Args[1:] {
		if a != "--register-service" && a != "--unregister-service" {
			args = append(args, a)
		}
	}

	s, err := m.CreateService(*flServiceName, p, c, args...)
	if err != nil {
		return err
	}
	defer s.Close()
	err = eventlog.Install(*flServiceName, p, false, eventlog.Info|eventlog.Warning|eventlog.Error)
	if err != nil {
		return err
	}

	return nil
}

func unregisterService() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := m.OpenService(*flServiceName)
	if err != nil {
		return err
	}
	defer s.Close()

	eventlog.Remove(*flServiceName)
	err = s.Delete()
	if err != nil {
		return err
	}
	return nil
}

func initService() (bool, error) {
	if *flUnregisterService {
		if *flRegisterService {
			return true, errors.New("--register-service and --unregister-service cannot be used together")
		}
		return true, unregisterService()
	}

	if *flRegisterService {
		return true, registerService()
	}

	if !*flRunService {
		return false, nil
	}

	interactive, err := svc.IsAnInteractiveSession()
	if err != nil {
		return false, err
	}

	h := &handler{
		tosvc:   make(chan bool),
		fromsvc: make(chan error),
	}

	var log *eventlog.Log
	if !interactive {
		log, err = eventlog.Open(*flServiceName)
		if err != nil {
			return false, err
		}
	}

	logrus.AddHook(&etwHook{log})
	logrus.SetOutput(ioutil.Discard)

	service = h
	go func() {
		if interactive {
			err = debug.Run(*flServiceName, h)
		} else {
			err = svc.Run(*flServiceName, h)
		}

		h.fromsvc <- err
	}()

	// Wait for the first signal from the service handler.
	err = <-h.fromsvc
	if err != nil {
		return false, err
	}
	return false, nil
}

func (h *handler) started() error {
	// This must be delayed until daemonCli initializes Config.Root
	err := initPanicFile(filepath.Join(daemonCli.Config.Root, "panic.log"))
	if err != nil {
		return err
	}

	h.tosvc <- false
	return nil
}

func (h *handler) stopped(err error) {
	logrus.Debugf("Stopping service: %v", err)
	h.tosvc <- err != nil
	<-h.fromsvc
}

func (h *handler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending, Accepts: 0}
	// Unblock initService()
	h.fromsvc <- nil

	// Wait for initialization to complete.
	failed := <-h.tosvc
	if failed {
		logrus.Debugf("Aborting service start due to failure during initializtion")
		return true, 1
	}

	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown | svc.Accepted(windows.SERVICE_ACCEPT_PARAMCHANGE)}
	logrus.Debugf("Service running")
Loop:
	for {
		select {
		case failed = <-h.tosvc:
			break Loop
		case c := <-r:
			switch c.Cmd {
			case svc.Cmd(windows.SERVICE_CONTROL_PARAMCHANGE):
				daemonCli.reloadConfig()
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s <- svc.Status{State: svc.StopPending, Accepts: 0}
				daemonCli.stop()
			}
		}
	}

	removePanicFile()
	if failed {
		return true, 1
	}
	return false, 0
}

func initPanicFile(path string) error {
	var err error
	panicFile, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}

	st, err := panicFile.Stat()
	if err != nil {
		return err
	}

	// If there are contents in the file already, move the file out of the way
	// and replace it.
	if st.Size() > 0 {
		panicFile.Close()
		os.Rename(path, path+".old")
		panicFile, err = os.Create(path)
		if err != nil {
			return err
		}
	}

	// Update STD_ERROR_HANDLE to point to the panic file so that Go writes to
	// it when it panics. Remember the old stderr to restore it before removing
	// the panic file.
	sh := syscall.STD_ERROR_HANDLE
	h, err := syscall.GetStdHandle(sh)
	if err != nil {
		return err
	}

	oldStderr = h

	r, _, err := setStdHandle.Call(uintptr(sh), uintptr(panicFile.Fd()))
	if r == 0 && err != nil {
		return err
	}

	return nil
}

func removePanicFile() {
	if st, err := panicFile.Stat(); err == nil {
		if st.Size() == 0 {
			sh := syscall.STD_ERROR_HANDLE
			setStdHandle.Call(uintptr(sh), uintptr(oldStderr))
			panicFile.Close()
			os.Remove(panicFile.Name())
		}
	}
}
