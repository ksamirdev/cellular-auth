// Command cell runs one cell: an independent authorization service with its
// own database.
package main

import (
	"github.com/caarlos0/env/v11"
	"go.uber.org/fx"

	"github.com/ksamirdev/cellular-auth/internal/cell"
	"github.com/ksamirdev/cellular-auth/internal/platform"
)

func main() {
	fx.New(
		fx.Provide(env.ParseAs[cell.Config]),
		platform.Module,
		cell.Module,
	).Run()
}
