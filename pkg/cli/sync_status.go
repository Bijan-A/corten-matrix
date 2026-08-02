package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"go.mau.fi/util/dbutil"
	"gopkg.in/yaml.v3"

	"github.com/lrhodin/corten-matrix/pkg/connector"
)

// syncStatusConfig captures just enough of config.yaml to open the same
// database the running bridge uses. It intentionally does not use
// bridgeconfig.Config: that struct pulls in the full framework config
// validation path, which this read-only status query doesn't need and
// shouldn't be coupled to.
type syncStatusConfig struct {
	Database struct {
		Type string `yaml:"type"`
		URI  string `yaml:"uri"`
	} `yaml:"database"`
}

// runSyncStatus implements `corten-matrix sync-status` (and `sync-status 1`
// for the second account). It opens the bridge's database directly — no
// running daemon required — and prints the same report the in-room
// `sync-status` management command shows, computed by the shared
// connector.GetSyncStatus.
func runSyncStatus(args []string) {
	dir := cortenDataDir()
	if len(args) > 0 && args[0] == "1" {
		dir = secondDataDir()
	}
	configPath := filepath.Join(dir, "config.yaml")

	data, err := os.ReadFile(configPath)
	if err != nil {
		die("Could not read %s: %v", configPath, err)
	}
	var cfg syncStatusConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		die("Could not parse %s: %v", configPath, err)
	}
	if cfg.Database.Type == "" || cfg.Database.URI == "" {
		die("No database configured in %s", configPath)
	}

	db, err := dbutil.NewWithDialect(cfg.Database.URI, cfg.Database.Type)
	if err != nil {
		die("Could not open database: %v", err)
	}
	defer db.RawDB.Close()

	// bridgeID is always "" — corten-matrix never overrides bridgev2's
	// default bridge ID (see mxmain's NewBridge("", ...) call).
	report, err := connector.GetSyncStatus(context.Background(), db, "")
	if err != nil {
		die("Could not read sync status (has the bridge logged in yet?): %v", err)
	}
	fmt.Print(report.Format(nil))
}
