// Command router runs the global transaction router in front of the cells.
package main

import (
	"github.com/caarlos0/env/v11"
	"go.uber.org/fx"

	"github.com/ksamirdev/cellular-auth/internal/platform"
	"github.com/ksamirdev/cellular-auth/internal/router"
)

func main() {
	fx.New(
		fx.Provide(env.ParseAs[router.Config]),
		platform.Module,
		router.Module,
	).Run()
}
