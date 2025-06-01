// main.go
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	// Namespace flags for Cloneflags
	CLONE_NEWUTS = syscall.CLONE_NEWUTS
	CLONE_NEWPID = syscall.CLONE_NEWPID
	CLONE_NEWNS  = syscall.CLONE_NEWNS
	CLONE_NEWNET = syscall.CLONE_NEWNET
	CLONE_NEWIPC = syscall.CLONE_NEWIPC
)

func main() {
	// If first argument is "init", run containerInit(); otherwise enter "runtime" mode.
	if len(os.Args) > 1 && os.Args[1] == "init" {
		if err := containerInit(); err != nil {
			log.Fatalf("container init failed: %v", err)
		}
		return
	}

	// Runtime mode: parse flags, fork/exec child with new namespaces.
	runCmd := flag.NewFlagSet("run", flag.ExitOnError)
	rootfs := runCmd.String("rootfs", "", "Path to the directory to use as root filesystem (required)")
	memLimit := runCmd.String("mem", "", "Memory limit (e.g. 100m, 1g). If empty, no limit is applied.")
	hostname := runCmd.String("hostname", "mini-container", "Hostname to set inside the container")
	runCmd.Parse(os.Args[1:])

	if *rootfs == "" {
		log.Fatal("Error: --rootfs must be specified")
	}
	remaining := runCmd.Args()
	if len(remaining) == 0 {
		log.Fatal("Error: must specify at least one command to run inside the container")
	}

	cmdPath, err := exec.LookPath(os.Args[0])
	if err != nil {
		log.Fatalf("failed to find self executable: %v", err)
	}

	// Build the command for the child: re-exec self with “init” marker
	childArgs := append([]string{"init"}, remaining...)
	cmd := exec.Command(cmdPath, childArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Pass rootfs, mem limit, and desired hostname via environment
	cmd.Env = append(os.Environ(),
		"ROOTFS="+*rootfs,
		"MEMLIMIT="+*memLimit,
		"HOSTNAME="+*hostname,
	)

	// Unshare UTS, PID, Mount, Network, IPC namespaces
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: uintptr(
			CLONE_NEWUTS |
				CLONE_NEWPID |
				CLONE_NEWNS |
				CLONE_NEWNET |
				CLONE_NEWIPC,
		),
	}

	log.Printf("[runtime] starting child process in new namespaces")
	if err := cmd.Start(); err != nil {
		log.Fatalf("failed to start child process: %v", err)
	}

	childPid := cmd.Process.Pid
	log.Printf("[runtime] child PID: %d", childPid)

	// If a memory limit was specified, apply it via cgroup v1
	if *memLimit != "" {
		limitBytes, err := parseMemLimit(*memLimit)
		if err != nil {
			log.Printf("[runtime] warning: could not parse memory limit %q: %v", *memLimit, err)
		} else {
			if err := applyMemoryCgroupLimit(childPid, limitBytes); err != nil {
				log.Printf("[runtime] warning: failed to apply memory cgroup limit: %v", err)
			} else {
				log.Printf("[runtime] applied memory limit %d bytes to PID %d", limitBytes, childPid)
			}
		}
	}

	// Wait for the containerized process to exit, and propagate its exit code
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		log.Fatalf("error waiting for child process: %v", err)
	}
}

// containerInit runs inside the child after namespaces are unshared.
func containerInit() error {
	// 1) Read environment variables
	newRoot := os.Getenv("ROOTFS")
	if newRoot == "" {
		return fmt.Errorf("ROOTFS not set")
	}
	memLimit := os.Getenv("MEMLIMIT") // may be empty
	hostname := os.Getenv("HOSTNAME") // e.g. "mini-container"

	// 2) Set hostname inside UTS namespace
	if hostname != "" {
		if err := syscall.Sethostname([]byte(hostname)); err != nil {
			return fmt.Errorf("sethostname(%q): %w", hostname, err)
		}
	}

	// 3) Make sure mounts below are private so that unmounts stay in this namespace
	if err := syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("remount / as private: %w", err)
	}

	// 4) Pivot_root (or fallback to chroot) into newRoot
	if err := pivotRoot(newRoot); err != nil {
		return fmt.Errorf("pivotRoot: %w", err)
	}

	// 5) Mount /proc inside the new root
	if err := mountProc(); err != nil {
		return fmt.Errorf("mountProc: %w", err)
	}

	// 6) Bring up loopback interface inside new net namespace (best-effort)
	if err := setupLoopback(); err != nil {
		log.Printf("[container] warning: failed to bring up loopback: %v", err)
	}

	// 7) (Optional) If memLimit is still set, you could double-check cgroup here
	//    But typically parent has already placed the child in the right cgroup.

	// 8) Exec the user’s command (everything after “init”)
	if len(os.Args) < 3 {
		return fmt.Errorf("no command provided for container to run")
	}
	cmdPath := os.Args[2]
	cmdArgs := os.Args[2:]
	if err := syscall.Exec(cmdPath, cmdArgs, os.Environ()); err != nil {
		return fmt.Errorf("exec %q %v: %w", cmdPath, cmdArgs, err)
	}
	return nil
}

// pivotRoot moves the current root to newRoot and makes newRoot “/”.
// It creates a temporary directory “.pivot_root” inside newRoot to hold the old root.
func pivotRoot(newRoot string) error {
	absRoot, err := filepath.Abs(newRoot)
	if err != nil {
		return fmt.Errorf("failed to get absolute path of newRoot %q: %w", newRoot, err)
	}

	putOld := filepath.Join(absRoot, ".pivot_root")
	if err := os.MkdirAll(putOld, 0700); err != nil {
		return fmt.Errorf("mkdir %q: %w", putOld, err)
	}

	// 1) Bind-mount newRoot onto itself to ensure it's a mount point
	if err := syscall.Mount(absRoot, absRoot, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("mount --bind %q onto itself: %w", absRoot, err)
	}

	// 2) pivot_root(newRoot, newRoot/.pivot_root)
	if err := syscall.PivotRoot(absRoot, putOld); err != nil {
		return fmt.Errorf("pivot_root(%q, %q): %w", absRoot, putOld, err)
	}

	// 3) Change working directory to new root
	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("chdir / after pivot: %w", err)
	}

	// 4) Unmount old root (now at /.pivot_root)
	oldRoot := "/.pivot_root"
	if err := syscall.Unmount(oldRoot, syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount %q: %w", oldRoot, err)
	}

	// 5) Remove the temporary directory
	if err := os.RemoveAll(oldRoot); err != nil {
		return fmt.Errorf("removeAll %q: %w", oldRoot, err)
	}

	return nil
}

// mountProc mounts a new procfs at /proc.
func mountProc() error {
	// Ensure /proc exists
	if err := os.MkdirAll("/proc", 0555); err != nil {
		return fmt.Errorf("mkdir /proc: %w", err)
	}
	// mount("proc", "/proc", "proc", 0, "")
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		return fmt.Errorf("mount procfs: %w", err)
	}
	return nil
}

// setupLoopback is a best-effort attempt to bring up the loopback interface inside the new net namespace.
// We exec "ip link set lo up" if the "ip" binary is present.
func setupLoopback() error {
	ipPath, err := exec.LookPath("ip")
	if err != nil {
		// If "ip" isn't available, try "ifconfig lo up"
		ifconfigPath, ifErr := exec.LookPath("ifconfig")
		if ifErr != nil {
			return fmt.Errorf("neither 'ip' nor 'ifconfig' found to bring up loopback")
		}
		cmd := exec.Command(ifconfigPath, "lo", "up")
		return cmd.Run()
	}

	cmd := exec.Command(ipPath, "link", "set", "lo", "up")
	return cmd.Run()
}

// parseMemLimit parses strings like "100m", "1g", "512k" into bytes.
func parseMemLimit(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty memory limit")
	}
	s = strings.TrimSpace(s)
	unit := s[len(s)-1]
	mult := int64(1)

	switch unit {
	case 'k', 'K':
		mult = 1024
		s = s[:len(s)-1]
	case 'm', 'M':
		mult = 1024 * 1024
		s = s[:len(s)-1]
	case 'g', 'G':
		mult = 1024 * 1024 * 1024
		s = s[:len(s)-1]
	default:
		// If last character is not a unit, assume bytes
		if unit < '0' || unit > '9' {
			return 0, fmt.Errorf("invalid memory limit suffix %q", string(unit))
		}
	}

	base, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse integer from %q: %w", s, err)
	}
	return base * mult, nil
}

// applyMemoryCgroupLimit creates a memory cgroup under cgroup v1 and limits the given PID.
// Requires that /sys/fs/cgroup/memory is mounted and writable (and that the runtime has permissions).
func applyMemoryCgroupLimit(pid int, limitBytes int64) error {
	// e.g. /sys/fs/cgroup/memory/mini_<pid>
	cgroupBase := "/sys/fs/cgroup/memory"
	if _, err := os.Stat(cgroupBase); err != nil {
		return fmt.Errorf("%q not found or not accessible: %w", cgroupBase, err)
	}

	cgroupPath := filepath.Join(cgroupBase, fmt.Sprintf("mini_%d", pid))
	if err := os.Mkdir(cgroupPath, 0755); err != nil {
		return fmt.Errorf("mkdir %q: %w", cgroupPath, err)
	}

	limitPath := filepath.Join(cgroupPath, "memory.limit_in_bytes")
	if err := os.WriteFile(limitPath, []byte(strconv.FormatInt(limitBytes, 10)), 0644); err != nil {
		return fmt.Errorf("write %q: %w", limitPath, err)
	}

	procsPath := filepath.Join(cgroupPath, "cgroup.procs")
	if err := os.WriteFile(procsPath, []byte(strconv.Itoa(pid)), 0644); err != nil {
		return fmt.Errorf("write %q: %w", procsPath, err)
	}

	return nil
}
