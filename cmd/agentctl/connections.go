package main

import (
	"fmt"
	"strings"

	"github.com/vishal0589/gstreco-tally-agent/internal/config"
)

func resolveConfigPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return config.DefaultPath()
	}
	return path
}

func requireAllConnections(cfg *config.Config) ([]config.PairedConnection, error) {
	conns := cfg.PairedConnections()
	if len(conns) == 0 {
		return nil, fmt.Errorf("config exists but has no valid paired connections")
	}
	return conns, nil
}

func requireOneConnection(
	cfg *config.Config,
	requestedConnectionID string,
) (config.PairedConnection, error) {
	conns, err := requireAllConnections(cfg)
	if err != nil {
		return config.PairedConnection{}, err
	}
	requestedConnectionID = strings.TrimSpace(requestedConnectionID)
	if requestedConnectionID == "" {
		if len(conns) == 1 {
			return conns[0], nil
		}
		ids := make([]string, 0, len(conns))
		for _, conn := range conns {
			ids = append(ids, conn.ConnectionID)
		}
		return config.PairedConnection{}, fmt.Errorf(
			"multiple connections configured; pass --connection-id (available: %s)",
			strings.Join(ids, ", "),
		)
	}
	conn, ok := cfg.FindPairedConnection(requestedConnectionID)
	if !ok {
		return config.PairedConnection{}, fmt.Errorf(
			"unknown connection_id %q in config", requestedConnectionID,
		)
	}
	return conn, nil
}
