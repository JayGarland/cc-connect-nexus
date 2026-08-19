package nexus

import (
	"github.com/chenhg5/cc-connect/core"
	"github.com/chenhg5/cc-connect/extensions/nexus/recovery"
)

func init() {
	core.RegisterPlatform("telegram", NewTelegramWrapper)
	core.RegisterDefaultCronRecoveryDecider(recovery.NewNexusCronRecoveryDecider)
}
