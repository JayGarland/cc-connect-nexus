package nexus

import (
	"github.com/chenhg5/cc-connect/core"
)

func init() {
	core.RegisterPlatform("telegram", NewTelegramWrapper)
}
