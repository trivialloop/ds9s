package main

import (
	"flag"
	"fmt"
	"os"

	"ds9s/internal/config"
	"ds9s/internal/ui"
)

func main() {
	var (
		configPath = flag.String("config", "", "path to ds9s config file (default: ~/.config/ds9s/config.yaml)")
		manager    = flag.String("manager", "", "name of the manager to connect to (overrides config's 'current')")
	)
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ds9s:", err)
		os.Exit(1)
	}
	if *manager != "" {
		cfg.Current = *manager
	}

	app, err := ui.NewApp(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ds9s:", err)
		os.Exit(1)
	}
	if err := app.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "ds9s:", err)
		os.Exit(1)
	}
}
