package main

import (
	"fmt"
	"testing"
)

func TestSqliteLinkClassifies(t *testing.T) {
	// exact joined form from tone's make -Bn (continuations joined, tabs intact)
	line := "cc -o sqlite3  \t/home/house/brine-worker/repos/sqlite/build/shell.c /home/house/brine-worker/repos/sqlite/build/sqlite3.c -Wl,-rpath,/usr/local/lib  -lm  -lz     \t \t-I. -DSQLITE_ENABLE_MATH_FUNCTIONS -DSQLITE_THREADSAFE=1 -DHAVE_READLINE=1 -DSQLITE_HAVE_ZLIB=1 \t-O2 -g  -I/usr/include  \t -lreadline -lncurses"
	fields := splitCommand(line)
	fmt.Printf("fields[0]=%q n=%d\n", fields[0], len(fields))
	l := classifyLink(fields)
	if l == nil {
		t.Fatalf("sqlite3 combined compile+link did not classify\nfields: %v", fields)
	}
	fmt.Printf("classified: out=%q objects=%v defines=%v\n", l.Out, l.Objects, l.Defines)
	if l.Out != "sqlite3" {
		t.Errorf("out: %q", l.Out)
	}
}
