// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package debugimage

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jaegertracing/jaeger/internal/testutils"
)

func TestMain(m *testing.M) {
	testutils.VerifyGoLeaks(m)
}

// marker is the value the debuggee keeps in memory. It is a placeholder, not a credential.
const marker = "placeholder-stored-trace"

// delveVersion is the module version built into the debug base image
// ghcr.io/jaegertracing/base-image-with-debugger:0.2.0 pinned by
// scripts/build/docker/debug/Dockerfile. That image's go.mod requires
// github.com/go-delve/delve v1.27.1 and builds cmd/dlv.
const delveVersion = "v1.27.1"

// TestDebugImageRejectsUnauthenticatedRemoteClients fails while a published
// image entrypoint starts Delve listening on a non-loopback address without
// authentication. A client in another network namespace must not be able to
// read the debuggee's memory or execute a function inside it.
func TestDebugImageRejectsUnauthenticatedRemoteClients(t *testing.T) {
	root := repoRoot(t)
	stages := delveStages(t, root)
	if len(stages) == 0 {
		t.Log("no image entrypoint starts Delve")
		return
	}

	dlvBin, attacker := buildDelveClient(t)
	victim := buildVictim(t, root)
	ns, serverIP := newAttackNetNS(t)

	for _, stage := range stages {
		t.Run(stage.id, func(t *testing.T) {
			// Run the entrypoint as written. A loopback listen must still be
			// executed: the remote namespace dials the non-loopback address,
			// which succeeds only when the socket is not confined to loopback.
			port, _ := tcpListen(stage.entry)
			if port == "" {
				joined := strings.Join(stage.entry, " ")
				if strings.Contains(joined, "listen") && !strings.Contains(joined, "unix:") {
					t.Fatalf("could not parse listen address from %q", stage.entry)
				}
				t.Logf("entrypoint %q does not open a TCP debugger", stage.entry)
				return
			}
			side := filepath.Join(t.TempDir(), "side-effect")
			logPath := filepath.Join(t.TempDir(), "dlv.log")
			cmd := startDelve(t, dlvBin, victim, stage.entry, logPath)
			waitForListen(t, cmd, logPath)

			outPath := filepath.Join(t.TempDir(), "result.json")
			addr := net.JoinHostPort(serverIP, port)
			attack := exec.Command("sudo", "-n", "ip", "netns", "exec", ns, attacker,
				"-addr", addr, "-side", side, "-out", outPath)
			attackOut, attackErr := attack.CombinedOutput()
			res := readResult(t, outPath)
			if attackErr != nil && !res.Connected && res.DialError == "" {
				t.Fatalf("attacker failed before reporting a result: %v\n%s", attackErr, attackOut)
			}

			sideEffect := false
			if st, err := os.Stat(side); err == nil && st.Mode().IsRegular() {
				sideEffect = true
			}
			memory := res.EvalValue == marker || res.Examined == marker || res.SecondEval == marker
			if !memory && !sideEffect {
				t.Logf("remote client uid %d (server uid %d) was refused: dial=%s eval=%s call=%s",
					res.UID, os.Getuid(), res.DialError, res.EvalError, res.CallError)
				return
			}
			t.Fatalf("entrypoint %q in %s allows an unauthenticated client at %s (client uid %d, server uid %d) to debug the process: memoryRead=%v eval=%q examined=%q secondClient=%v secondEval=%q codeExecuted=%v callErr=%s",
				stage.entry, stage.file, addr, res.UID, os.Getuid(),
				memory, res.EvalValue, res.Examined, res.SecondClient, res.SecondEval, sideEffect, res.CallError)
		})
	}
}

type result struct {
	Connected    bool   `json:"connected"`
	UID          int    `json:"uid"`
	DialError    string `json:"dialError,omitempty"`
	SecondClient bool   `json:"secondClient"`
	SecondEval   string `json:"secondEval,omitempty"`
	EvalValue    string `json:"evalValue,omitempty"`
	Examined     string `json:"examined,omitempty"`
	EvalError    string `json:"evalError,omitempty"`
	ExamineError string `json:"examineError,omitempty"`
	CallError    string `json:"callError,omitempty"`
}

type stage struct {
	id    string
	file  string
	name  string
	entry []string
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func delveStages(t *testing.T, root string) []stage {
	t.Helper()
	var stages []stage
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "jaeger-ui", "idl", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "Dockerfile" {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		require.NoError(t, err)
		mentioned := 0
		for _, line := range strings.Split(string(content), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
				continue
			}
			if strings.EqualFold(fields[0], "ENTRYPOINT") && strings.Contains(line, "dlv") {
				mentioned++
			}
		}
		found := 0
		for _, st := range parseStages(t, rel, string(content)) {
			if !invokesDelve(st.entry) {
				continue
			}
			st.file = rel
			st.id = rel + "/" + st.name
			stages = append(stages, st)
			found++
		}
		if mentioned != found {
			t.Fatalf("%s has %d Delve entrypoints but the parser found %d", rel, mentioned, found)
		}
		return nil
	})
	require.NoError(t, err)
	return stages
}

func parseStages(t *testing.T, file, dockerfile string) []stage {
	t.Helper()
	var stages []stage
	var cur *stage
	for _, line := range strings.Split(dockerfile, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "FROM":
			stages = append(stages, stage{name: fields[len(fields)-1]})
			cur = &stages[len(stages)-1]
		case "ENTRYPOINT":
			if cur == nil {
				continue
			}
			rest := strings.TrimSpace(line[len(fields[0]):])
			if !strings.HasPrefix(rest, "[") {
				if strings.Contains(rest, "dlv") {
					t.Fatalf("%s has a shell-form Delve entrypoint; the test does not know its arguments: %s", file, rest)
				}
				continue
			}
			var argv []string
			if err := json.Unmarshal([]byte(rest), &argv); err != nil {
				t.Fatalf("%s entrypoint is not a JSON array: %s", file, rest)
			}
			cur.entry = argv
		default:
			// Other instructions do not change the command the image runs.
		}
	}
	return stages
}

func invokesDelve(entry []string) bool {
	for _, arg := range entry {
		if filepath.Base(arg) == "dlv" || strings.Contains(arg, "/dlv") {
			return true
		}
	}
	return false
}

// tcpListen reports the port and whether the entrypoint serves the debugger
// on a non-loopback TCP address. Delve's default listen address is loopback,
// and --headless (or the dap subcommand) is what opens the TCP listener.
func tcpListen(entry []string) (string, bool) {
	headless := false
	dap := false
	listen := ""
	for i, arg := range entry {
		switch {
		case arg == "--headless":
			headless = true
		case arg == "dap":
			dap = true
		case strings.HasPrefix(arg, "--listen="):
			listen = strings.TrimPrefix(arg, "--listen=")
		case (arg == "--listen" || arg == "-l") && i+1 < len(entry):
			listen = entry[i+1]
		default:
			// Program arguments and unrelated flags do not choose the listen address.
		}
	}
	if listen == "" || strings.HasPrefix(listen, "unix:") || (!headless && !dap) {
		return "", false
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return port, false
	}
	return port, true
}

func buildVictim(t *testing.T, root string) string {
	t.Helper()
	dir := t.TempDir()
	src, err := os.ReadFile(filepath.Join(root, "cmd", "jaeger", "debugimage", "testdata", "victim", "main.go.txt"))
	require.NoError(t, err)
	require.Contains(t, string(src), marker)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), src, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module victim\n\ngo 1.27.0\n"), 0o644))
	bin := filepath.Join(dir, "victim")
	// Match scripts/makefiles/BuildBinaries.mk's debug-binary flags so the
	// symbols survive the way they do in jaeger-debug-linux-*.
	cmd := exec.Command("go", "build", "-trimpath", "-gcflags=all=-N -l", "-o", bin, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off", "GOTOOLCHAIN=go1.27.0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build victim: %s", out)
	return bin
}

func buildDelveClient(t *testing.T) (dlvBin, attackerBin string) {
	t.Helper()
	dir := t.TempDir()
	mod := "module dlvbuild\n\ngo 1.27.0\n\nrequire github.com/go-delve/delve " + delveVersion + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644))
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	src, err := os.ReadFile(filepath.Join(filepath.Dir(file), "testdata", "attacker", "main.go.txt"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "attacker.go"), src, 0o644))
	get := exec.Command("go", "get", "github.com/go-delve/delve/cmd/dlv@"+delveVersion)
	get.Dir = dir
	get.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=go1.27.0")
	out, err := get.CombinedOutput()
	require.NoError(t, err, "go get delve: %s", out)

	dlv := filepath.Join(dir, "dlv")
	build := exec.Command("go", "build", "-mod=mod", "-o", dlv, "github.com/go-delve/delve/cmd/dlv")
	build.Dir = dir
	build.Env = get.Env
	out, err = build.CombinedOutput()
	require.NoError(t, err, "build dlv: %s", out)

	attacker := filepath.Join(dir, "attacker")
	buildAtk := exec.Command("go", "build", "-mod=mod", "-o", attacker, "attacker.go")
	buildAtk.Dir = dir
	buildAtk.Env = get.Env
	out, err = buildAtk.CombinedOutput()
	require.NoError(t, err, "build attacker: %s", out)
	return dlv, attacker
}

func newAttackNetNS(t *testing.T) (nsName, serverIP string) {
	t.Helper()
	serverIP = "10.201.31.1"
	attackerIP := "10.201.31.2"
	nsName = fmt.Sprintf("jdlv%d", os.Getpid())
	vethS := fmt.Sprintf("vjs%d", os.Getpid()%100000)
	vethA := fmt.Sprintf("vja%d", os.Getpid()%100000)

	sudoMayFail("ip", "netns", "del", nsName)
	sudoMayFail("ip", "link", "del", vethS)
	sudo(t, "ip", "netns", "add", nsName)
	sudo(t, "ip", "link", "add", vethS, "type", "veth", "peer", "name", vethA)
	sudo(t, "ip", "link", "set", vethA, "netns", nsName)
	sudo(t, "ip", "addr", "add", serverIP+"/30", "dev", vethS)
	sudo(t, "ip", "link", "set", vethS, "up")
	sudo(t, "ip", "netns", "exec", nsName, "ip", "addr", "add", attackerIP+"/30", "dev", vethA)
	sudo(t, "ip", "netns", "exec", nsName, "ip", "link", "set", vethA, "up")
	sudo(t, "ip", "netns", "exec", nsName, "ip", "link", "set", "lo", "up")
	t.Cleanup(func() {
		sudoMayFail("ip", "netns", "del", nsName)
		sudoMayFail("ip", "link", "del", vethS)
	})
	return nsName, serverIP
}

func sudo(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("sudo", append([]string{"-n"}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "sudo %s: %s", strings.Join(args, " "), out)
}

func sudoMayFail(args ...string) {
	cmd := exec.Command("sudo", append([]string{"-n"}, args...)...)
	_ = cmd.Run()
}

func startDelve(t *testing.T, dlv, victim string, entry []string, logPath string) *exec.Cmd {
	t.Helper()
	argv := append([]string{}, entry...)
	replacedBin := false
	for i, arg := range argv {
		if filepath.Base(arg) == "dlv" {
			argv[i] = dlv
		}
		if arg == "exec" && i+1 < len(argv) {
			argv[i+1] = victim
			replacedBin = true
		}
	}
	require.True(t, replacedBin, "entrypoint has no exec target: %q", entry)
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = logFile.Close() })

	// argv[0] is the dlv path inside the image. Drop it; Command sets the binary.
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

func waitForListen(t *testing.T, cmd *exec.Cmd, logPath string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(logPath)
		require.NoError(t, err)
		if strings.Contains(string(body), "API server listening") || strings.Contains(string(body), "couldn't start listener") {
			require.Contains(t, string(body), "API server listening", "delve log:\n%s", body)
			return
		}
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("delve exited before listening:\n%s", body)
		}
		time.Sleep(50 * time.Millisecond)
	}
	body, _ := os.ReadFile(logPath)
	t.Fatalf("delve did not listen within 20s:\n%s", body)
}

func readResult(t *testing.T, path string) result {
	t.Helper()
	body, err := os.ReadFile(path)
	require.NoError(t, err, "attacker wrote no result")
	var res result
	require.NoError(t, json.Unmarshal(body, &res), "result: %s", body)
	return res
}
