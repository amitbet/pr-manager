//go:build desktop

package main

import (
	"context"
	_ "embed"

	"github.com/wailsapp/wails/v2"
	wailsOptions "github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	wailsRuntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const desktopBuild = true

// Linux takes the window icon from here. macOS and Windows read it from the
// app bundle and the exe resources that scripts/build-desktop.sh adds.
//
//go:embed assets/icon/icon.png
var desktopIcon []byte

// runDesktop quits when ctx is done (SIGINT/SIGTERM). Quitting cancels the
// running jobs and waits briefly for them, so their LLM CLIs, which run in
// their own process groups, are killed rather than orphaned.
func runDesktop(ctx context.Context, o options) error {
	inheritShellPath()
	handler, stopJobs, err := newServeHandler(o)
	if err != nil {
		return err
	}
	defer stopJobs() // in case Run returns without calling OnShutdown
	quit := make(chan struct{})
	defer close(quit)
	return wails.Run(&wailsOptions.App{
		Title:       "PR Manager",
		Width:       1280,
		Height:      800,
		MinWidth:    900,
		MinHeight:   600,
		AssetServer: &assetserver.Options{Handler: handler},
		Linux:       &linux.Options{Icon: desktopIcon},
		OnStartup: func(wctx context.Context) {
			go func() {
				select {
				case <-ctx.Done():
					wailsRuntime.Quit(wctx)
				case <-quit:
				}
			}()
		},
		OnShutdown: func(context.Context) { stopJobs() },
	})
}
