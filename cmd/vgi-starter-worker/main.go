// Command vgi-starter-worker serves the starter catalog to DuckDB.
//
//	vgi-starter-worker                                  # stdio (what DuckDB launches)
//	vgi-starter-worker --http --host 0.0.0.0 --port 8000  # HTTP, GET /health
//	vgi-starter-worker --unix /tmp/starter.sock         # AF_UNIX (launch: transport)
//
// Attach it from DuckDB:
//
//	ATTACH 'starter' (TYPE vgi, LOCATION '/abs/path/to/bin/vgi-starter-worker');
package main

import (
	"os"

	"github.com/Query-farm/vgi-go/vgi"

	"github.com/Query-farm/vgi-starter-go/internal/serve"
	"github.com/Query-farm/vgi-starter-go/internal/starter"
)

func main() {
	serve.Main("vgi-starter-worker", os.Args[1:], func() *vgi.Worker { return starter.NewWorker() })
}
