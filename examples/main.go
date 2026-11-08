// Command recipes runs one recipe as a worker: recipes <recipe> [worker flags].
//
//	ATTACH 'aggregate' (TYPE vgi, LOCATION '/abs/bin/recipes aggregate');
//
// Each recipe is one file in this directory and builds its own *vgi.Worker.
// The recipes are deliberately small and skip the full metadata the main
// worker carries.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Query-farm/vgi-go/vgi"

	"github.com/your-org/vgi-hue/internal/serve"
)

// recipes maps a recipe name to the function that builds its worker.
var recipes = map[string]func() *vgi.Worker{
	"transform_rows":       transformRowsWorker,
	"aggregate":            aggregateWorker,
	"pushdown_sqlite":      pushdownSQLiteWorker,
	"cache_expensive_call": cacheExpensiveCallWorker,
	"catalog_tables":       catalogTablesWorker,
	"copy_to_sink":         copyToSinkWorker,
}

func main() {
	if len(os.Args) < 2 || recipes[os.Args[1]] == nil {
		names := make([]string, 0, len(recipes))
		for name := range recipes {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Fprintf(os.Stderr, "usage: recipes <%s> [worker flags]\n", strings.Join(names, "|"))
		os.Exit(2)
	}
	serve.Main("recipes "+os.Args[1], os.Args[2:], recipes[os.Args[1]])
}
