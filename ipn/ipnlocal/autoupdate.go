// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package ipnlocal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/version"
)

func (b *LocalBackend) stopOfflineAutoUpdate() {
	b.offlineAutoUpdateMu.Lock()
	defer b.offlineAutoUpdateMu.Unlock()
	if b.offlineAutoUpdateCancel != nil {
		b.logf("offline auto-update: stopping update checks")
		b.offlineAutoUpdateCancel()
		b.offlineAutoUpdateCancel = nil
	}
}

func (b *LocalBackend) maybeStartOfflineAutoUpdate(prefs ipn.PrefsView) {
	if !prefs.AutoUpdate().Apply.EqualBool(true) {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	b.offlineAutoUpdateMu.Lock()
	defer b.offlineAutoUpdateMu.Unlock()
	if b.offlineAutoUpdateCancel != nil {
		// Already running.
		return
	}
	b.offlineAutoUpdateCancel = cancel

	b.logf("offline auto-update: starting update checks")
	go b.offlineAutoUpdate(ctx)
}

const offlineAutoUpdateCheckPeriod = time.Hour

func (b *LocalBackend) offlineAutoUpdate(ctx context.Context) {
	t := time.NewTicker(offlineAutoUpdateCheckPeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := b.startAutoUpdate("offline auto-update"); err != nil {
			b.logf("offline auto-update: failed: %v", err)
		}
	}
}

// startAutoUpdate triggers an auto-update attempt. The actual update happens
// asynchronously. If another update is in progress, an error is returned.
func (b *LocalBackend) startAutoUpdate(logPrefix string) (retErr error) {
	// Check if update was already started, and mark as started.
	if !b.trySetC2NUpdateStarted() {
		return errors.New("update already started")
	}
	defer func() {
		// Clear the started flag if something failed.
		if retErr != nil {
			b.setC2NUpdateStarted(false)
		}
	}()

	cmdTS, err := findCmdTailscale()
	if err != nil {
		return fmt.Errorf("failed to find cmd/tailscale binary: %w", err)
	}
	var ver struct {
		Long string `json:"long"`
	}
	out, err := exec.Command(cmdTS, "version", "--json").Output()
	if err != nil {
		return fmt.Errorf("failed to find cmd/tailscale binary: %w", err)
	}
	if err := json.Unmarshal(out, &ver); err != nil {
		return fmt.Errorf("invalid JSON from cmd/tailscale version --json: %w", err)
	}
	if ver.Long != version.Long() {
		return fmt.Errorf("cmd/tailscale version %q does not match tailscaled version %q", ver.Long, version.Long())
	}

	cmd := tailscaleUpdateCmd(cmdTS)
	buf := new(bytes.Buffer)
	cmd.Stdout = buf
	cmd.Stderr = buf
	b.logf("%s: running %q", logPrefix, strings.Join(cmd.Args, " "))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start cmd/tailscale update: %w", err)
	}

	go func() {
		if err := cmd.Wait(); err != nil {
			b.logf("%s: update command failed: %v, output: %s", logPrefix, err, buf)
		} else {
			b.logf("%s: update attempt complete", logPrefix)
		}
		b.setC2NUpdateStarted(false)
	}()
	return nil
}
