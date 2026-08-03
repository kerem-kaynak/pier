package awsec2

import (
	"strings"

	"github.com/kerem-kaynak/pier/internal/driver"
)

// Machines is the TUI resize picker's catalog, filtered to the session's CPU
// architecture (resize can't cross arch: the disk's binaries live on).
// Costs are rough eu on-demand rates. Anything not listed here still works
// through `pier resize <session> <type>`.
func Machines(currentType string) []driver.Machine {
	family, _, _ := strings.Cut(currentType, ".")
	if strings.HasSuffix(family, "g") { // t4g, m7g, ... = arm (Graviton)
		return []driver.Machine{
			{Type: "t4g.small", CPU: "2", Mem: "2", Cost: "~$0.02/h"},
			{Type: "t4g.medium", CPU: "2", Mem: "4", Cost: "~$0.04/h"},
			{Type: "t4g.large", CPU: "2", Mem: "8", Cost: "~$0.08/h"},
			{Type: "t4g.xlarge", CPU: "4", Mem: "16", Cost: "~$0.15/h"},
			{Type: "t4g.2xlarge", CPU: "8", Mem: "32", Cost: "~$0.31/h"},
			{Type: "m7g.4xlarge", CPU: "16", Mem: "64", Cost: "~$0.65/h"},
		}
	}
	return []driver.Machine{
		{Type: "t3.small", CPU: "2", Mem: "2", Cost: "~$0.02/h"},
		{Type: "t3.medium", CPU: "2", Mem: "4", Cost: "~$0.05/h"},
		{Type: "t3.large", CPU: "2", Mem: "8", Cost: "~$0.10/h"},
		{Type: "t3.xlarge", CPU: "4", Mem: "16", Cost: "~$0.19/h"},
		{Type: "t3.2xlarge", CPU: "8", Mem: "32", Cost: "~$0.38/h"},
		{Type: "m7i.4xlarge", CPU: "16", Mem: "64", Cost: "~$0.86/h"},
	}
}
