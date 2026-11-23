package main

import "testing"

func TestSplitCompoundAndLibtool(t *testing.T) {
	// exact curl shape: echo label; /bin/sh ../libtool --mode=compile gcc ...
	line := `echo "  CC      " libcurl_la-altsvc.lo;/bin/sh ../libtool --silent --tag=CC   --mode=compile gcc -DHAVE_CONFIG_H -I../include -DBUILDING_LIBCURL -c altsvc.c -o libcurl_la-altsvc.o`
	segs := splitCompound(line)
	if len(segs) != 1 {
		t.Fatalf("segments: %v", segs)
	}
	fields := splitCommand(segs[0])
	unwrapped := unwrapLibtool(fields)
	if unwrapped == nil {
		t.Fatal("libtool not unwrapped")
	}
	c := classifyCompile(unwrapped)
	if c == nil {
		t.Fatalf("embedded compile not classified: %v", unwrapped)
	}
	if c.Src != "altsvc.c" || c.Obj != "libcurl_la-altsvc.o" {
		t.Errorf("src/obj: %q/%q", c.Src, c.Obj)
	}
	if !contains(c.Defines, "BUILDING_LIBCURL") {
		t.Errorf("defines: %v", c.Defines)
	}

	// link mode
	linkLine := `echo "  CCLD    " libcurl.la;/bin/sh ../libtool --silent --tag=CC   --mode=link gcc  -fvisibility=hidden -O2 -version-info 12:0:8 -o libcurl.la libcurl_la-altsvc.lo libcurl_la-amigaos.lo -lz`
	lfields := splitCommand(splitCompound(linkLine)[0])
	lunwrapped := unwrapLibtool(lfields)
	l := classifyLink(lunwrapped)
	if l == nil {
		t.Fatalf("embedded link not classified: %v", lunwrapped)
	}
	if l.Out != "libcurl.la" {
		t.Errorf("out: %q", l.Out)
	}
}
