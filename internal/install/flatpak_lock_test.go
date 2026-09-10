//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || windows

package install

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFlatpakInstallLockConcurrent(t *testing.T) {
	optsA, sourceA, app := flatpakFixture(t)
	optsB, sourceB, _ := flatpakFixture(t)
	optsB.Home = optsA.Home
	flatpakWrite(t, optsA.ExePath, "host a", 0o755)
	flatpakWrite(t, filepath.Join(sourceA, "background.js"), "worker a", 0o644)
	flatpakWrite(t, optsB.ExePath, "host b", 0o755)
	flatpakWrite(t, filepath.Join(sourceB, "background.js"), "worker b", 0o644)
	if _, err := InstallChromeFlatpak(optsA, sourceA); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(app, flatpakLockName)
	lockInfo, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && lockInfo.Mode().Perm() != 0o600 {
		t.Fatalf("lock permissions = %o; want 600", lockInfo.Mode().Perm())
	}
	for i := range 30 {
		start := make(chan struct{})
		results := make(chan error, 2)
		for _, input := range []struct {
			opts   Options
			source string
		}{{optsA, sourceA}, {optsB, sourceB}} {
			go func() {
				<-start
				_, err := InstallChromeFlatpak(input.opts, input.source)
				results <- err
			}()
		}
		close(start)
		succeeded := 0
		for range 2 {
			if err := <-results; err == nil {
				succeeded++
			} else if !errors.Is(err, errFlatpakInstallBusy) {
				t.Errorf("iteration %d: unexpected installer error: %v", i, err)
			}
		}
		if succeeded == 0 {
			t.Fatalf("iteration %d: neither installer succeeded", i)
		}
		host, err := os.ReadFile(filepath.Join(app, "data/tailtab/bin/tailtab"))
		if err != nil || (string(host) != "host a" && string(host) != "host b") {
			t.Fatalf("iteration %d: installed host = %q, %v", i, host, err)
		}
		flatpakCheck(t, filepath.Join(app, "data/tailtab/extension/background.js"), "worker "+strings.TrimPrefix(string(host), "host "))
		flatpakNoStage(t, app)
		after, err := os.Stat(lockPath)
		if err != nil || !os.SameFile(lockInfo, after) {
			t.Fatalf("iteration %d: persistent lock inode changed: %v", i, err)
		}
	}
}

func TestFlatpakInstallLockHeld(t *testing.T) {
	opts, source, app := flatpakFixture(t)
	lockPath := filepath.Join(app, flatpakLockName)
	flatpakWrite(t, lockPath, "persistent lock contents", 0o600)
	if _, err := InstallChromeFlatpak(opts, source); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(app, "data/tailtab/extension/stale.js")
	flatpakWrite(t, stale, "preserve old asset", 0o644)
	before := make(map[string]os.FileInfo)
	contents := make(map[string]string)
	for _, path := range []string{lockPath, stale, filepath.Join(app, "data/tailtab/bin/tailtab"), filepath.Join(app, "data/tailtab/extension/background.js"), filepath.Join(app, "config/google-chrome/NativeMessagingHosts", manifestFile)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path], contents[path] = info, string(data)
	}
	root, err := os.OpenRoot(app)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	release, err := lockFlatpakApp(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	flatpakWrite(t, opts.ExePath, "replacement host", 0o755)
	flatpakWrite(t, filepath.Join(source, "background.js"), "replacement worker", 0o644)
	if paths, err := InstallChromeFlatpak(opts, source); !errors.Is(err, errFlatpakInstallBusy) || len(paths) != 0 {
		t.Fatalf("held lock: paths = %v, error = %v; want busy", paths, err)
	}
	for path, info := range before {
		after, err := os.Stat(path)
		if err != nil || !os.SameFile(info, after) || !info.ModTime().Equal(after.ModTime()) {
			t.Fatalf("busy installer changed %s: %v", path, err)
		}
		// Windows locks also prevent reads of the locked byte through another handle.
		if path != lockPath {
			flatpakCheck(t, path, contents[path])
		}
	}
	flatpakNoStage(t, app)
	release()
	release = nil
	if _, err := InstallChromeFlatpak(opts, source); err != nil {
		t.Fatalf("retry after release: %v", err)
	}
	flatpakCheck(t, filepath.Join(app, "data/tailtab/bin/tailtab"), "replacement host")
	flatpakCheck(t, filepath.Join(app, "data/tailtab/extension/background.js"), "replacement worker")
	flatpakCheck(t, lockPath, "persistent lock contents")
	after, err := os.Stat(lockPath)
	if err != nil || !os.SameFile(before[lockPath], after) {
		t.Fatalf("lock inode changed after release: %v", err)
	}
	flatpakNoStage(t, app)
}

func TestFlatpakInstallLockProcess(t *testing.T) {
	if app := os.Getenv("TAILTAB_FLATPAK_LOCK_HELPER"); app != "" {
		root, err := os.OpenRoot(app)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		release, err := lockFlatpakApp(root)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if _, err := fmt.Fprint(os.Stdout, "L"); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			t.Fatal(err)
		}
		// Bypass deferred unlock/close to verify the OS releases the lock on exit.
		os.Exit(0)
	}
	opts, source, app := flatpakFixture(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestFlatpakInstallLockProcess$", "-test.timeout=15s")
	cmd.Env = append(os.Environ(), "TAILTAB_FLATPAK_LOCK_HELPER="+app)
	cmd.Stderr = os.Stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = input.Close()
		select {
		case <-done:
			if waitErr != nil {
				t.Errorf("lock helper exited: %v", waitErr)
			}
		case <-time.After(20 * time.Second):
			t.Error("lock helper did not exit within 20 seconds")
		}
	})
	ready := make(chan error, 1)
	go func() {
		var marker [1]byte
		_, err := io.ReadFull(output, marker[:])
		if err == nil && marker[0] != 'L' {
			err = fmt.Errorf("unexpected helper readiness marker %q", marker)
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("waiting for held lock: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("lock helper did not become ready within 10 seconds")
	}
	if paths, err := InstallChromeFlatpak(opts, source); !errors.Is(err, errFlatpakInstallBusy) || len(paths) != 0 {
		t.Fatalf("subprocess-held lock: paths = %v, error = %v; want busy", paths, err)
	}
	if _, err := os.Stat(filepath.Join(app, "data")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("busy installer changed app data: %v", err)
	}
	flatpakNoStage(t, app)
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if waitErr != nil {
			t.Fatalf("lock helper exited: %v", waitErr)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("lock helper did not exit within 20 seconds")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := InstallChromeFlatpak(opts, source)
		if err == nil {
			break
		}
		// Windows can release a terminated process's locks shortly after Wait returns.
		if runtime.GOOS != "windows" || !errors.Is(err, errFlatpakInstallBusy) || time.Now().After(deadline) {
			t.Fatalf("retry after process exit: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	flatpakCheck(t, filepath.Join(app, "data/tailtab/bin/tailtab"), "host v1")
	flatpakNoStage(t, app)
}

func TestFlatpakInstallLockRejectsUnsafeEntries(t *testing.T) {
	for _, kind := range []string{"inside symlink", "outside symlink", "dangling symlink", "directory", "socket"} {
		t.Run(kind, func(t *testing.T) {
			opts, source, app := flatpakFixture(t)
			host := filepath.Join(app, "data/tailtab/bin/tailtab")
			worker := filepath.Join(app, "data/tailtab/extension/background.js")
			flatpakWrite(t, host, "old host", 0o755)
			flatpakWrite(t, worker, "old worker", 0o644)
			lockPath := filepath.Join(app, flatpakLockName)
			target := filepath.Join(app, "lock-target")
			switch kind {
			case "directory":
				target = filepath.Join(lockPath, "keep")
				flatpakWrite(t, target, "untouched", 0o600)
			case "socket":
				if runtime.GOOS == "windows" {
					t.Skip("Unix socket fixture")
				}
				t.Chdir(app)
				listener, err := net.Listen("unix", flatpakLockName)
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
			default:
				if kind != "inside symlink" {
					target = filepath.Join(filepath.Dir(opts.Home), "lock-target")
				}
				if kind != "dangling symlink" {
					flatpakWrite(t, target, "untouched", 0o600)
				}
				linkTarget := target
				if kind == "inside symlink" {
					linkTarget = "lock-target"
				}
				if err := os.Symlink(linkTarget, lockPath); err != nil {
					if runtime.GOOS == "windows" {
						t.Skipf("symlinks unavailable: %v", err)
					}
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			if paths, err := InstallChromeFlatpak(opts, source); err == nil || errors.Is(err, errFlatpakInstallBusy) || !strings.Contains(err.Error(), flatpakLockName) || len(paths) != 0 {
				t.Fatalf("accepted unsafe lock entry: %v, %v", paths, err)
			}
			after, err := os.Lstat(lockPath)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatalf("lock entry changed: %v", err)
			}
			if kind == "dangling symlink" {
				if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("dangling link target changed: %v", err)
				}
			} else if kind != "socket" {
				flatpakCheck(t, target, "untouched")
			}
			flatpakCheck(t, host, "old host")
			flatpakCheck(t, worker, "old worker")
			flatpakNoStage(t, app)
		})
	}
}
