// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build linux

// Package ptracer holds the start command of CWS injector
package ptracer

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/vmihailenco/msgpack/v5"
	"golang.org/x/sys/unix"

	"github.com/DataDog/datadog-agent/pkg/security/proto/ebpfless"
	"github.com/DataDog/datadog-agent/pkg/security/secl/model"
	"github.com/DataDog/datadog-agent/pkg/util/native"
)

const (
	defaultPasswdPath = "/etc/passwd"
	defaultGroupPath  = "/etc/group"
	// EnvPasswdPathOverride define the env to set to override the default passwd file path
	EnvPasswdPathOverride = "TEST_DD_PASSWD_PATH"
	// EnvGroupPathOverride define the env to set to override the default group file path
	EnvGroupPathOverride = "TEST_DD_GROUP_PATH"
)

var (
	passwdPath = defaultPasswdPath
	groupPath  = defaultGroupPath
)

// Opts defines ptracer options
type Opts struct {
	Creds            Creds
	Verbose          bool
	Async            bool
	StatsDisabled    bool
	ProcScanDisabled bool
	ScanProcEvery    time.Duration
	SeccompDisabled  bool
	AttachedCb       func()

	// internal
	mode               ebpfless.Mode
	entryPointArgs     []string
	capturePtracerProc bool
}

type syscallHandlerFunc func(tracer *Tracer, process *Process, msg *ebpfless.SyscallMsg, regs syscall.PtraceRegs, disableStats bool) error

type shouldSendFunc func(msg *ebpfless.SyscallMsg) bool

type syscallID struct {
	ID   int
	Name string
}

type syscallHandler struct {
	IDs        []syscallID        // IDs defines the list of syscall IDs related to this handler
	Func       syscallHandlerFunc // Func defines the entrance handler for those syscalls, can be nil
	ShouldSend shouldSendFunc     // ShouldSend checks if we should send the event regarding the syscall return value. If nil, acts as false
	RetFunc    syscallHandlerFunc // RetFunc defines the return handler for those syscalls, can be nil
}

// defaults funcs for ShouldSend:
func shouldSendAlways(_ *ebpfless.SyscallMsg) bool { return true }

func isAcceptedRetval(msg *ebpfless.SyscallMsg) bool {
	return msg.Retval >= 0 || msg.Retval == -int64(syscall.EACCES) || msg.Retval == -int64(syscall.EPERM)
}

func checkEntryPoint(path string) (string, error) {
	name, err := exec.LookPath(path)
	if err != nil {
		return "", err
	}

	name, err = filepath.Abs(name)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(name)
	if err != nil {
		return "", err
	}

	if !info.Mode().IsRegular() {
		return "", errors.New("entrypoint not a regular file")
	}

	if info.Mode()&0111 == 0 {
		return "", errors.New("entrypoint not an executable")
	}

	return name, nil
}

func initConn(probeAddr string, nbAttempts uint) (net.Conn, error) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", probeAddr)
	if err != nil {
		return nil, err
	}

	var (
		client net.Conn
	)

	err = retry.Do(func() error {
		client, err = net.DialTCP("tcp", nil, tcpAddr)
		return err
	}, retry.Delay(time.Second), retry.Attempts(nbAttempts))
	if err != nil {
		return nil, err
	}
	return client, nil
}

func sendMsgData(client net.Conn, data []byte) error {
	// write size
	var size [4]byte
	native.Endian.PutUint32(size[:], uint32(len(data)))
	if _, err := client.Write(size[:]); err != nil {
		return fmt.Errorf("unabled to send size: %v", err)
	}

	if _, err := client.Write(data); err != nil {
		return fmt.Errorf("unabled to send message: %v", err)
	}
	return nil
}

func registerSyscallHandlers() (map[int]syscallHandler, []string) {
	handlers := make(map[int]syscallHandler)

	syscalls := registerFIMHandlers(handlers)
	syscalls = append(syscalls, registerProcessHandlers(handlers)...)
	syscalls = append(syscalls, registerERPCHandlers(handlers)...)

	return handlers, syscalls
}

// Attach attach the ptracer
func Attach(pids []int, probeAddr string, opts Opts) error {
	logger := Logger{opts.Verbose}

	logger.Debugf("Attach to pid %+v [%s] using `standard` mode", pids, os.Getenv("DD_CONTAINER_ID"))

	if path := os.Getenv(EnvPasswdPathOverride); path != "" {
		passwdPath = path
	}
	if path := os.Getenv(EnvGroupPathOverride); path != "" {
		groupPath = path
	}

	syscallHandlers, _ := registerSyscallHandlers()

	tracerOpts := TracerOpts{
		Creds:           opts.Creds,
		Logger:          logger,
		SeccompDisabled: true, // force to true, can't use seccomp with the attach mode
	}

	tracer, err := AttachTracer(pids, tracerOpts)
	if err != nil {
		return err
	}

	opts.mode = ebpfless.AttachedMode
	opts.capturePtracerProc = true

	return ptrace(tracer, probeAddr, syscallHandlers, logger, opts)
}

// Wrap the executable
func Wrap(args []string, envs []string, probeAddr string, opts Opts) error {
	logger := Logger{opts.Verbose}

	if len(args) == 0 {
		return fmt.Errorf("an executable is required")
	}
	entry, err := checkEntryPoint(args[0])
	if err != nil {
		return err
	}

	mode := "seccomp"
	if opts.SeccompDisabled {
		mode = "standard"
	}

	logger.Debugf("Run %s %v [%s] using `%s` mode", entry, args, os.Getenv("DD_CONTAINER_ID"), mode)

	if path := os.Getenv(EnvPasswdPathOverride); path != "" {
		passwdPath = path
	}
	if path := os.Getenv(EnvGroupPathOverride); path != "" {
		groupPath = path
	}

	syscallHandlers, syscalls := registerSyscallHandlers()

	tracerOpts := TracerOpts{
		Syscalls:        syscalls,
		Creds:           opts.Creds,
		Logger:          logger,
		SeccompDisabled: opts.SeccompDisabled,
	}

	tracer, err := NewTracer(entry, args, envs, tracerOpts)
	if err != nil {
		return err
	}

	opts.mode = ebpfless.WrappedMode
	opts.entryPointArgs = args

	return ptrace(tracer, probeAddr, syscallHandlers, logger, opts)
}

// startPtracer start the ptracer
func ptrace(tracer *Tracer, probeAddr string, syscallHandlers map[int]syscallHandler, logger Logger, opts Opts) error {
	if path := os.Getenv(EnvPasswdPathOverride); path != "" {
		passwdPath = path
	}
	if path := os.Getenv(EnvGroupPathOverride); path != "" {
		groupPath = path
	}

	var (
		client      net.Conn
		clientReady = make(chan bool, 1)
		wg          sync.WaitGroup
		err         error
	)

	connectClient := func() error {
		var err error
		client, err = initConn(probeAddr, 600)
		if err != nil {
			clientReady <- false
			logger.Errorf("connection to system-probe failed!")
			return err
		}
		clientReady <- true
		logger.Debugf("connection to system-probe initiated!")
		return nil
	}

	if probeAddr != "" {
		logger.Debugf("connection to system-probe...")
		if opts.Async {
			go func() {
				_ = connectClient()
			}()
		} else {
			err = connectClient()
			if err != nil {
				return err
			}
		}
	}

	containerID, err := getCurrentProcContainerID()
	if err != nil {
		logger.Errorf("Retrieve container ID from proc failed: %v\n", err)
	}
	containerCtx, err := newContainerContext(containerID)
	if err != nil {
		return err
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	go func() {
		<-sigChan
		os.Exit(0)
	}()

	msgDataChan := make(chan []byte, 100000)

	send := func(msg *ebpfless.Message) {
		logger.Debugf("sending message: %s", msg)

		if probeAddr == "" {
			return
		}

		data, err := msgpack.Marshal(msg)
		if err != nil {
			logger.Errorf("unable to marshal message: %v", err)
			return
		}

		select {
		case msgDataChan <- data:
		default:
			logger.Errorf("unable to send message")
		}
	}

	pc := NewProcessCache()

	// first processes
	for _, pid := range tracer.PIDs {
		process := NewProcess(pid)
		pc.Add(pid, process)
	}

	// collect tracees via proc
	if opts.capturePtracerProc {
		for _, pid := range tracer.PIDs {
			proc, err := collectProcess(int32(pid))
			if err != nil {
				return err
			}

			if msg, err := procToMsg(proc); err == nil {
				send(msg)
			}
		}
	}

	ctx, cancelFnc := context.WithCancel(context.Background())

	if probeAddr == "" {
		send(&ebpfless.Message{
			Type: ebpfless.MessageTypeHello,
			Hello: &ebpfless.HelloMsg{
				Mode:             opts.mode,
				NSID:             getNSID(),
				ContainerContext: containerCtx,
				EntrypointArgs:   opts.entryPointArgs,
			},
		})
	} else /* probeAddr != "" */ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for {
			LOOP: // wait for the client to be ready or stopped
				for {
					select {
					case <-ctx.Done():
						return
					case ready := <-clientReady:
						if !ready {
							time.Sleep(time.Second)
							// re-init connection
							logger.Debugf("try to reconnect to system-probe...")
							go func() {
								_ = connectClient()
							}()
							continue
						}

						defer client.Close()

						// if ready, send an hello message
						helloMsg := &ebpfless.Message{
							Type: ebpfless.MessageTypeHello,
							Hello: &ebpfless.HelloMsg{
								Mode:             opts.mode,
								NSID:             getNSID(),
								ContainerContext: containerCtx,
								EntrypointArgs:   opts.entryPointArgs,
							},
						}
						logger.Debugf("sending message: %s", helloMsg)
						data, err := msgpack.Marshal(helloMsg)
						if err != nil {
							logger.Errorf("unable to marshal message: %v", err)
							return
						}
						if err = sendMsgData(client, data); err != nil {
							logger.Debugf("error sending hallo msg: %v", err)
							go func() {
								_ = connectClient()
							}()
							continue
						}
						break LOOP
					}
				}

			LOOP2: // unqueue and try to send messages or wait client to be stopped
				for {
					select {
					case data := <-msgDataChan:
						if err := sendMsgData(client, data); err != nil {
							logger.Errorf("error sending msg: %v", err)
							msgDataChan <- data
							break LOOP2
						}
					case <-ctx.Done():
						return
					}
				}

				// re-init connection
				logger.Debugf("try to reconnect to system-probe...")
				go func() {
					_ = connectClient()
				}()
			}
		}()
	}

	if !opts.ProcScanDisabled {
		every := opts.ScanProcEvery
		if every == 0 {
			every = 500 * time.Millisecond
		}

		wg.Add(1)
		go func() {
			defer wg.Done()

			// introduce a delay before starting to scan procfs to let the tracer event first
			time.Sleep(2 * time.Second)

			scanProcfs(ctx, tracer, send, every, logger)
		}()
	}

	cb := func(cbType CallbackType, nr int, pid int, ppid int, regs syscall.PtraceRegs, waitStatus *syscall.WaitStatus) {
		handler, found := syscallHandlers[nr]
		if !found && !slices.Contains([]int{ExecveNr, ExecveatNr, IoctlNr, CloneNr, Clone3Nr, ForkNr, VforkNr, ExitNr}, nr) {
			return
		}

		process := pc.Get(pid)
		if process == nil {
			process = NewProcess(pid)
			pc.Add(pid, process)
		}

		sendSyscallMsg := func(msg *ebpfless.SyscallMsg) {
			if msg == nil {
				return
			}
			msg.PID = uint32(process.Tgid)
			msg.Timestamp = uint64(time.Now().UnixNano())
			msg.ContainerID = containerID
			send(&ebpfless.Message{
				Type:    ebpfless.MessageTypeSyscall,
				Syscall: msg,
			})
		}

		switch cbType {
		case CallbackPreType:
			syscallMsg := &ebpfless.SyscallMsg{}
			if nr == ExecveatNr {
				// special case: sometimes, execveat returns as execve, to handle that, we force
				// the msg to be put in ExecveNr
				process.Nr[ExecveNr] = syscallMsg
			} else {
				process.Nr[nr] = syscallMsg
			}

			if handler.Func != nil {
				err := handler.Func(tracer, process, syscallMsg, regs, opts.StatsDisabled)
				if err != nil {
					return
				}
			}

			// if available, gather span
			syscallMsg.SpanContext = fillSpanContext(tracer, process.Tgid, pid, pc.GetSpan(process.Tgid))

			/* internal special cases */
			switch nr {
			case ExecveNr:
				// Top level pids, add opts.Creds. For the other PIDs the creds will be propagated at the probe side
				for _, pid := range tracer.PIDs {
					if process.Pid == pid {
						var uid, gid uint32

						if opts.Creds.UID != nil {
							uid = *opts.Creds.UID
						} else {
							uid = uint32(os.Getuid())
						}

						if opts.Creds.GID != nil {
							gid = *opts.Creds.GID
						} else {
							gid = uint32(os.Getgid())
						}

						syscallMsg.Exec.Credentials = &ebpfless.Credentials{
							UID:  uid,
							EUID: uid,
							GID:  gid,
							EGID: gid,
						}
						if !opts.StatsDisabled {
							syscallMsg.Exec.Credentials.User = getUserFromUID(tracer, int32(syscallMsg.Exec.Credentials.UID))
							syscallMsg.Exec.Credentials.EUser = getUserFromUID(tracer, int32(syscallMsg.Exec.Credentials.EUID))
							syscallMsg.Exec.Credentials.Group = getGroupFromGID(tracer, int32(syscallMsg.Exec.Credentials.GID))
							syscallMsg.Exec.Credentials.EGroup = getGroupFromGID(tracer, int32(syscallMsg.Exec.Credentials.EGID))
						}
					}
				}

				// special case for exec since the pre reports the pid while the post reports the tgid
				if process.Pid != process.Tgid {
					pc.Add(process.Tgid, process)
				}
			case ExecveatNr:
				// special case for exec since the pre reports the pid while the post reports the tgid
				if process.Pid != process.Tgid {
					pc.Add(process.Tgid, process)
				}
			case IoctlNr:
				req := handleERPC(tracer, process, regs)
				if len(req) != 0 {
					if isTLSRegisterRequest(req) {
						pc.SetSpanTLS(process.Tgid, handleTLSRegister(req))
					}
				}
			}
		case CallbackPostType:
			syscallMsg, msgExists := process.Nr[nr]
			if msgExists {
				if handler.RetFunc != nil {
					err := handler.RetFunc(tracer, process, syscallMsg, regs, opts.StatsDisabled)
					if err != nil {
						return
					}
				}
				if handler.ShouldSend != nil {
					syscallMsg.Retval = tracer.ReadRet(regs)
					if handler.ShouldSend(syscallMsg) && syscallMsg.Type != ebpfless.SyscallTypeUnknown {
						sendSyscallMsg(syscallMsg)
					}
				}
			}

			/* internal special cases */
			switch nr {
			case ExecveNr, ExecveatNr:
				// now the pid is the tgid
				process.Pid = process.Tgid
				// remove previously registered TLS
				pc.UnsetSpan(process.Tgid)
			case CloneNr:
				if flags := tracer.ReadArgUint64(regs, 0); flags&uint64(unix.SIGCHLD) == 0 {
					pc.SetAsThreadOf(process, ppid)
				} else if parent := pc.Get(ppid); parent != nil {
					sendSyscallMsg(&ebpfless.SyscallMsg{
						Type: ebpfless.SyscallTypeFork,
						Fork: &ebpfless.ForkSyscallMsg{
							PPID: uint32(parent.Tgid),
						},
					})
				}
			case Clone3Nr:
				data, err := tracer.ReadArgData(process.Pid, regs, 0, 8 /*sizeof flags only*/)
				if err != nil {
					return
				}
				if flags := binary.NativeEndian.Uint64(data); flags&uint64(unix.SIGCHLD) == 0 {
					pc.SetAsThreadOf(process, ppid)
				} else if parent := pc.Get(ppid); parent != nil {
					sendSyscallMsg(&ebpfless.SyscallMsg{
						Type: ebpfless.SyscallTypeFork,
						Fork: &ebpfless.ForkSyscallMsg{
							PPID: uint32(parent.Tgid),
						},
					})
				}
			case ForkNr, VforkNr:
				if parent := pc.Get(ppid); parent != nil {
					sendSyscallMsg(&ebpfless.SyscallMsg{
						Type: ebpfless.SyscallTypeFork,
						Fork: &ebpfless.ForkSyscallMsg{
							PPID: uint32(parent.Tgid),
						},
					})
				}
			}

		case CallbackExitType:
			// send exit only for process not threads
			if process.Pid == process.Tgid && waitStatus != nil {
				exitCtx := &ebpfless.ExitSyscallMsg{}
				if waitStatus.Exited() {
					exitCtx.Cause = model.ExitExited
					exitCtx.Code = uint32(waitStatus.ExitStatus())
				} else if waitStatus.CoreDump() {
					exitCtx.Cause = model.ExitCoreDumped
					exitCtx.Code = uint32(waitStatus.Signal())
				} else if waitStatus.Signaled() {
					exitCtx.Cause = model.ExitSignaled
					exitCtx.Code = uint32(waitStatus.Signal())
				} else {
					exitCtx.Code = uint32(waitStatus.Signal())
				}

				sendSyscallMsg(&ebpfless.SyscallMsg{
					Type: ebpfless.SyscallTypeExit,
					Exit: exitCtx,
				})
			}

			pc.Remove(process)
		}
	}

	defer func() {
		// stop client and msg chan reader
		cancelFnc()
		wg.Wait()
		close(msgDataChan)
	}()

	if opts.AttachedCb != nil {
		opts.AttachedCb()
	}

	if err := tracer.Trace(cb); err != nil {
		return err
	}

	// let a few queued message being send
	time.Sleep(time.Second)

	return nil
}
