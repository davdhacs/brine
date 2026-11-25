package main

import (
	"fmt"
	"testing"
)

func TestCollectorCommandFormat(t *testing.T) {
	// exact first line from collector's compile_commands.json conversion
	line := `cd /tmp/collector/cmake-build/collector && /usr/bin/c++ -DASSERT_TO_LOG -DSCAP_SOCKET_ONLY_FD -I/tmp/collector/collector/lib -o CMakeFiles/collector.dir/collector.cpp.o -c /tmp/collector/collector/collector.cpp`

	segs := splitCompound(line)
	fmt.Printf("segments: %d\n", len(segs))
	for i, s := range segs {
		fields := splitCommand(s)
		fmt.Printf("  seg %d: %d fields, first=%q\n", i, len(fields), fields[0])
		for j, f := range fields {
			fmt.Printf("    field[%d] = %q\n", j, f)
		}
		c := classifyCompile(fields)
		if c != nil {
			fmt.Printf("    -> compile: src=%q obj=%q\n", c.Src, c.Obj)
		} else {
			l := classifyLink(fields)
			if l != nil {
				fmt.Printf("    -> link: out=%q\n", l.Out)
			} else {
				fmt.Printf("    -> NOT CLASSIFIED\n")
			}
		}
	}
}
