// Command import-legacy migrates the v1 (Python era) douyin-archive database
// into the v2 schema and links the already-downloaded files into the new data
// directory.
//
// Typical run (from backend/):
//
//	go run ./cmd/import-legacy -legacy-db "E:\home\douyin-archive\data\app.db"
//
// The legacy database is opened strictly read-only. By default files are
// linked, not copied: <data-dir>/downloads/legacy becomes a junction to the
// legacy downloads root, so the existing corpus plays with zero disk cost.
//
// The same functionality is reachable through the server binary:
//
//	douyin-server.exe -mode import-legacy -legacy-db ...
package main

import (
	"os"

	"douyin/backend/internal/legacy"
)

func main() {
	os.Exit(legacy.Main(os.Args[1:]))
}
