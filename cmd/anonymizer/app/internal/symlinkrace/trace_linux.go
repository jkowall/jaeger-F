// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package symlinkrace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Result is what the traced child did with linkPath.
type Result struct {
	Planted  bool
	ExitCode int
	Output   string
}

// ChildEnv is the parent environment without exposure-test variables.
func ChildEnv(extra ...string) []string {
	env := make([]string, 0, len(os.Environ())+len(extra))
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "JAEGER_ANON_EXPOSURE_") {
			continue
		}
		env = append(env, e)
	}
	return append(env, extra...)
}

// PlantBeforeOpen starts bin with args and env under ptrace. If the child
// enters openat (or openat2) for linkPath, the symlink to targetPath is
// created before that syscall runs. A stat of linkPath that happened earlier
// still observed "not exists".
func PlantBeforeOpen(bin string, args, env []string, linkPath, targetPath string) (Result, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	logFile, err := os.CreateTemp("", "symlinkrace-")
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(logFile.Name())
	defer logFile.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}

	reaped := false
	defer func() {
		if !reaped {
			_ = cmd.Process.Kill()
		}
	}()

	root := cmd.Process.Pid
	live := map[int]struct{}{root: {}}
	entered := map[int]bool{}
	optsSet := map[int]bool{}
	var res Result
	res.ExitCode = -1

	for len(live) > 0 {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WALL, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			out := readLog(logFile)
			return res, fmt.Errorf("wait for traced child: %w; output: %s", err, out)
		}
		if _, ok := live[pid]; !ok {
			live[pid] = struct{}{}
		}
		if ws.Exited() || ws.Signaled() {
			delete(live, pid)
			if pid == root {
				reaped = true
				if ws.Exited() {
					res.ExitCode = ws.ExitStatus()
				} else {
					res.ExitCode = -int(ws.Signal())
				}
			}
			continue
		}
		if !ws.Stopped() {
			continue
		}
		sig := ws.StopSignal()
		if !optsSet[pid] && (sig == syscall.SIGTRAP || sig == syscall.SIGSTOP) && ws.TrapCause() <= 0 {
			err = syscall.PtraceSetOptions(pid, syscall.PTRACE_O_TRACESYSGOOD|
				syscall.PTRACE_O_TRACECLONE|syscall.PTRACE_O_TRACEFORK|
				syscall.PTRACE_O_TRACEVFORK|syscall.PTRACE_O_TRACEEXEC)
			if err != nil {
				return res, fmt.Errorf("ptrace options for %d: %w", pid, err)
			}
			optsSet[pid] = true
			if err := syscall.PtraceSyscall(pid, 0); err != nil {
				return res, fmt.Errorf("resume %d: %w", pid, err)
			}
			continue
		}
		cause := ws.TrapCause()
		if cause == syscall.PTRACE_EVENT_CLONE || cause == syscall.PTRACE_EVENT_FORK || cause == syscall.PTRACE_EVENT_VFORK {
			if msg, err := syscall.PtraceGetEventMsg(pid); err == nil && msg != 0 {
				live[int(msg)] = struct{}{}
			}
			_ = syscall.PtraceSyscall(pid, 0)
			continue
		}
		if sig == syscall.SIGTRAP|0x80 {
			if !entered[pid] {
				entered[pid] = true
				if !res.Planted && openPathMatches(pid, linkPath) {
					if err := os.Symlink(targetPath, linkPath); err != nil {
						return res, fmt.Errorf("plant symlink: %w", err)
					}
					res.Planted = true
				}
			} else {
				entered[pid] = false
			}
			if err := syscall.PtraceSyscall(pid, 0); err != nil && !errors.Is(err, syscall.ESRCH) {
				return res, fmt.Errorf("step %d: %w", pid, err)
			}
			continue
		}
		if err := syscall.PtraceSyscall(pid, int(sig)); err != nil && !errors.Is(err, syscall.ESRCH) {
			return res, fmt.Errorf("deliver signal %d to %d: %w", sig, pid, err)
		}
	}

	res.Output = readLog(logFile)
	return res, nil
}

func readLog(f *os.File) string {
	_, _ = f.Seek(0, 0)
	b, _ := os.ReadFile(f.Name())
	if len(b) > 4000 {
		b = b[:4000]
	}
	return string(b)
}

func openPathMatches(pid int, linkPath string) bool {
	nr, args, ok := readSyscall(pid)
	if !ok || len(args) < 2 {
		return false
	}
	// openat(dirfd, path, flags, mode). openat2's path is the same argument.
	if nr != int64(syscall.SYS_OPENAT) && nr != 437 {
		return false
	}
	return readCString(pid, args[1]) == linkPath
}

func readSyscall(pid int) (int64, []uint64, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/syscall", pid))
	if err != nil {
		return 0, nil, false
	}
	fields := strings.Fields(string(b))
	// nr, six args, stack pointer, instruction pointer.
	if len(fields) < 8 {
		return 0, nil, false
	}
	nr, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, nil, false
	}
	args := make([]uint64, 0, 6)
	for i := 1; i <= 6; i++ {
		v, err := strconv.ParseUint(strings.TrimPrefix(fields[i], "0x"), 16, 64)
		if err != nil {
			return 0, nil, false
		}
		args = append(args, v)
	}
	return nr, args, true
}

func readCString(pid int, addr uint64) string {
	if addr == 0 {
		return ""
	}
	var buf []byte
	var word [8]byte
	for len(buf) < 4096 {
		if _, err := syscall.PtracePeekData(pid, uintptr(addr), word[:]); err != nil {
			break
		}
		for _, c := range word {
			if c == 0 {
				return string(buf)
			}
			buf = append(buf, c)
		}
		addr += 8
	}
	return string(buf)
}
