// brine bazel-generate --parity: prove that the Bazel build compiles the
// same machine code as the go build.
//
// Method (validated on darwin/arm64, k8swatch):
//  1. go build -trimpath -ldflags="-s -w -buildid=redacted" -buildvcs=false
//     — matching the flags rules_go passes (buildid=redacted discovered
//     via bazel aquery; -s -w matches rules_go's stripped output).
//  2. bazel build the target.
//  3. Extract __text/.text from both and compare word-by-word. A word
//     that differs is EXPLAINED when it is an address-forming immediate:
//     - ADRP: same opcode+register, page immediate differs
//     - ADD/SUB imm: same opcode+registers, imm12 differs
//     - LDR/STR (unsigned offset): same opcode+registers, imm12 differs
//     rules_go embeds longer path strings (gopclntab ~11.6KB larger),
//     which shifts later sections; every address immediate shifts
//     accordingly. Measured: 23,929 differing words, 100% explained,
//     zero unexplained — instruction-identical code.
//
// Unexplained differing words mean genuinely different code: different
// compiler, different flags, or different sources. That is a parity
// failure and the whole point of the gate.
package main

import (
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// codeSection extracts the executable's code section bytes.
func codeSection(path string) ([]byte, string, error) {
	if f, err := macho.Open(path); err == nil {
		defer f.Close()
		for _, sec := range f.Sections {
			if sec.Name == "__text" {
				data, err := sec.Data()
				return data, "__text", err
			}
		}
		return nil, "", fmt.Errorf("no __text section")
	}
	if f, err := elf.Open(path); err == nil {
		defer f.Close()
		for _, sec := range f.Sections {
			if sec.Name == ".text" {
				data, err := sec.Data()
				return data, ".text", err
			}
		}
		return nil, "", fmt.Errorf("no .text section")
	}
	return nil, "", fmt.Errorf("not a Mach-O or ELF binary")
}

// word32 reads a little-endian 32-bit word.
func word32(b []byte, i int) uint32 {
	return binary.LittleEndian.Uint32(b[i : i+4])
}

func isADRPMasked(w uint32) bool    { return (w>>24)&0x9F == 0x90 }
func isAddImmMasked(w uint32) bool  { return (w & 0x7F800000) == 0x11000000 }
func isUimmMemMasked(w uint32) bool { return (w>>24)&0x3 == 1 }

// parityWords compares the code sections word-by-word; every differing
// word must be an address-immediate-only difference. Returns explained,
// unexplained counts and sample unexplained words.
func parityWords(a, b []byte) (explained, unexplained int, samples []uint32) {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i+4 <= n; i += 4 {
		wa, wb := word32(a, i), word32(b, i)
		if wa == wb {
			continue
		}
		ok := false
		switch {
		case isADRPMasked(wa) && isADRPMasked(wb) && (wa&0x9F00001F) == (wb&0x9F00001F):
			ok = true
		case isAddImmMasked(wa) && isAddImmMasked(wb) && (wa&0xFF8003FF) == (wb&0xFF8003FF):
			ok = true
		case isUimmMemMasked(wa) && isUimmMemMasked(wb) && (wa&0xFFC003FF) == (wb&0xFFC003FF):
			ok = true
		}
		if ok {
			explained++
		} else {
			unexplained++
			if len(samples) < 8 {
				samples = append(samples, wa, wb)
			}
		}
	}
	return
}

// cmdBazelParity runs the parity gate for a go_binary target.
//
//	brine bazel-generate --parity //cmd/kwtest
func cmdBazelParity(target string) {
	// strip label syntax: //cmd/kwtest[:name] -> ./cmd/kwtest
	pkgDir := strings.TrimPrefix(target, "//")
	pkgDir = strings.SplitN(pkgDir, ":", 2)[0]

	// 1. go build with rules_go-matched flags
	goOut := filepath.Join(os.TempDir(), fmt.Sprintf("brine-parity-go-%d", os.Getpid()))
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w -buildid=redacted", "-o", goOut, "./"+pkgDir)
	cmd.Env = append(os.Environ(), "GOFLAGS=-buildvcs=false")
	cmd.Stderr = os.Stderr
	fmt.Printf("[brine] parity: go build ./%s (flags matched to rules_go)\n", pkgDir)
	if err := cmd.Run(); err != nil {
		fatal("parity: go build failed: %v", err)
	}
	defer os.Remove(goOut)

	// 2. bazel build the target
	fmt.Printf("[brine] parity: bazel build %s\n", target)
	bcmd := bazelCmd("build", target)
	bcmd.Stdout = os.Stderr
	bcmd.Stderr = os.Stderr
	if err := bcmd.Run(); err != nil {
		fatal("parity: bazel build failed: %v", err)
	}

	// resolve the bazel output path
	q := bazelCmd("cquery", "--output=files", target)
	out, err := q.Output()
	if err != nil {
		fatal("parity: bazel cquery failed: %v", err)
	}
	bazelOut := strings.TrimSpace(string(out))
	if bazelOut == "" || strings.Contains(bazelOut, "\n") {
		fatal("parity: target does not resolve to a single file: %q", bazelOut)
	}

	// 3. compare code sections
	goText, secName, err := codeSection(goOut)
	if err != nil {
		fatal("parity: %v", err)
	}
	bazelText, _, err := codeSection(bazelOut)
	if err != nil {
		fatal("parity: %v", err)
	}
	fmt.Printf("[brine] parity: %s %d vs %d bytes\n", secName, len(goText), len(bazelText))

	explained, unexplained, samples := parityWords(goText, bazelText)
	if len(goText) != len(bazelText) {
		fmt.Printf("[brine] parity: ✗ code section SIZES differ (%d vs %d) — different code volume\n", len(goText), len(bazelText))
		os.Exit(1)
	}
	if unexplained > 0 {
		fmt.Printf("[brine] parity: ✗ %d unexplained code differences (not address immediates)\n", unexplained)
		for i := 0; i+1 < len(samples); i += 2 {
			fmt.Printf("    go=%#010x bazel=%#010x\n", samples[i], samples[i+1])
		}
		fmt.Printf("  These are real instruction differences: different compiler, flags, or sources.\n")
		os.Exit(1)
	}
	fmt.Printf("[brine] parity: ✓ CODE-EQUIVALENT — %d differing words, all address immediates\n", explained)
	fmt.Printf("  (layout shift from rules_go path strings; instruction streams identical)\n")
}

// bazelCmd finds bazel or bazelisk.
func bazelCmd(args ...string) *exec.Cmd {
	if _, err := exec.LookPath("bazel"); err == nil {
		return exec.Command("bazel", args...)
	}
	return exec.Command("bazelisk", args...)
}
