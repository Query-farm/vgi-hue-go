// Command vgi-hue-worker serves the hue catalog to DuckDB.
//
//	vgi-hue-worker                                  # stdio (what DuckDB launches)
//	vgi-hue-worker --http --host 0.0.0.0 --port 8000  # HTTP, GET /health
//	vgi-hue-worker --unix /tmp/hue.sock         # AF_UNIX (launch: transport)
//
// Attach it from DuckDB:
//
//	ATTACH 'hue' (TYPE vgi, LOCATION '/abs/path/to/bin/vgi-hue-worker');
package main

import (
	"os"

	"github.com/Query-farm/vgi-go/vgi"

	"github.com/your-org/vgi-hue/internal/hue"
	"github.com/your-org/vgi-hue/internal/serve"
)

func main() {
	serve.Main("vgi-hue-worker", os.Args[1:], func() *vgi.Worker { return hue.NewWorker() })
}
