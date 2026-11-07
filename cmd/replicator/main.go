// Command replicator ships every cell's outbox to every other cell.
package main

import (
	"github.com/caarlos0/env/v11"
	"go.uber.org/fx"

	"github.com/ksamirdev/cellular-auth/internal/platform"
	"github.com/ksamirdev/cellular-auth/internal/replication"
)

func main() {
	fx.New(
		fx.Provide(env.ParseAs[replication.Config]),
		platform.Module,
		replication.Module,
	).Run()
}
