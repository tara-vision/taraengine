// Package measure reads the memory the loaded models hold: on the GPU through nvidia-smi, or as
// resident memory where no driver reports it. The router spawns one process per model, so a probe
// always sums the supervised process and everything below it.
package measure

import (
	"strconv"
	"strings"
)

// Descendants returns root and every process below it, from `ps -axo pid=,ppid=` output.
// Each process is listed once, so a cycle in the snapshot cannot make the walk run forever.
func Descendants(psOutput string, root int) []int {
	children := map[int][]int{}
	for _, line := range strings.Split(psOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		children[ppid] = append(children[ppid], pid)
	}
	out := []int{root}
	seen := map[int]bool{root: true}
	for i := 0; i < len(out); i++ {
		for _, child := range children[out[i]] {
			if !seen[child] {
				seen[child] = true
				out = append(out, child)
			}
		}
	}
	return out
}
