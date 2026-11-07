package router

import (
	"fmt"

	"go.uber.org/fx"

	"github.com/ksamirdev/cellular-auth/internal/platform"
)

var Module = fx.Module("router",
	fx.Provide(
		func(c Config) (platform.ListenAddr, error) {
			if len(c.Cells) < 2 {
				return "", fmt.Errorf("CELLS needs at least two cells, got %d", len(c.Cells))
			}
			return platform.ListenAddr(c.Addr), nil
		},
		NewCells,
		New,
		NewHandler,
	),
)
