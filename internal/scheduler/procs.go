package scheduler

import (
	"github.com/shirou/gopsutil/v4/process"
)

// treeRSS sums resident memory of the process and all its descendants,
// so the shell's few-MB footprint doesn't mask the real toolchain behind
// it (F5).
func treeRSS(pid int32) uint64 {
	p, err := process.NewProcess(pid)
	if err != nil {
		return 0
	}
	var total uint64
	if mi, err := p.MemoryInfo(); err == nil {
		total += mi.RSS
	}
	children, err := p.Children()
	if err != nil {
		return total // no children, or unavailable
	}
	for _, c := range children {
		total += treeRSS(c.Pid)
	}
	return total
}
