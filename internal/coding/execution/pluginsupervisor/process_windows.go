//go:build windows

package pluginsupervisor

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsProcessController struct{ job windows.Handle }

type jobObjectBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

func newProcessController() processController { return &windowsProcessController{} }

func (c *windowsProcessController) configure(command *exec.Cmd) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	c.job = job
	// The process is assigned to the pre-created Job Object while suspended.
	// No plugin code can run before attach completes.
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED,
	}
	return nil
}

func (c *windowsProcessController) attach(command *exec.Cmd) error {
	if c.job == 0 || command == nil || command.Process == nil {
		return errors.New("pluginsupervisor: Windows Job Object is not prepared")
	}
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(command.Process.Pid),
	)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(process) }()
	if err := windows.AssignProcessToJobObject(c.job, process); err != nil {
		return err
	}
	if err := resumeProcessThreads(uint32(command.Process.Pid)); err != nil {
		_ = windows.TerminateJobObject(c.job, 1)
		return err
	}
	return nil
}

func resumeProcessThreads(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return err
	}
	for {
		if entry.OwnerProcessID == pid {
			thread, openErr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if openErr != nil {
				return openErr
			}
			_, resumeErr := windows.ResumeThread(thread)
			_ = windows.CloseHandle(thread)
			if resumeErr != nil {
				return resumeErr
			}
		}
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return nil
			}
			return err
		}
	}
}

func (c *windowsProcessController) close() error {
	if c.job == 0 {
		return nil
	}
	err := windows.CloseHandle(c.job)
	c.job = 0
	return err
}

func (c *windowsProcessController) terminate(
	ctx context.Context,
	_ *exec.Cmd,
	_ <-chan struct{},
) (processTermination, error) {
	if c.job == 0 {
		return processTermination{}, ErrCleanupUncertain
	}
	active, err := c.activeProcesses()
	if err != nil {
		return processTermination{}, errors.Join(ErrCleanupUncertain, err)
	}
	if active == 0 {
		return processTermination{treeQuiesced: true}, nil
	}
	if err := windows.TerminateJobObject(c.job, 1); err != nil && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return processTermination{forced: true}, err
	}
	quiesced, err := c.waitForQuiescence(ctx)
	if err != nil {
		return processTermination{forced: true}, errors.Join(ErrCleanupUncertain, err)
	}
	if !quiesced {
		return processTermination{forced: true}, ErrCleanupUncertain
	}
	return processTermination{forced: true, treeQuiesced: true}, nil
}

func (c *windowsProcessController) activeProcesses() (uint32, error) {
	var info jobObjectBasicAccountingInformation
	if err := windows.QueryInformationJobObject(
		c.job,
		windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
		nil,
	); err != nil {
		return 0, err
	}
	return info.ActiveProcesses, nil
}

func (c *windowsProcessController) waitForQuiescence(ctx context.Context) (bool, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		active, err := c.activeProcesses()
		if err != nil {
			return false, err
		}
		if active == 0 {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
		}
	}
}
