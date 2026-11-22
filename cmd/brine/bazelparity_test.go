package main

import (
	"encoding/binary"
	"testing"
)

func words(ws ...uint32) []byte {
	b := make([]byte, 4*len(ws))
	for i, w := range ws {
		binary.LittleEndian.PutUint32(b[i*4:], w)
	}
	return b
}

// ARM64 encodings used in these tests:
//   ADRP X0, #page:  0x90000000 | (immhi<<5) | (immlo<<29) | Rd
//   ADD X0, X0, #n:  0x91000000 | (n<<10) | (Rn<<5) | Rd
//   LDR X0, [X0,#n]: 0xF9400000 | (n<<10) | (Rn<<5) | Rd
//   MOV X0, X1:      0xAA0003E0 (a non-address instruction)
//   NOP:             0xD503201F

func TestParityWordsIdentical(t *testing.T) {
	a := words(0x90000000, 0x91000000, 0xF9400000, 0xD503201F)
	explained, unexplained, _ := parityWords(a, a)
	if explained != 0 || unexplained != 0 {
		t.Errorf("identical: explained=%d unexplained=%d", explained, unexplained)
	}
}

func TestParityWordsAddressShifts(t *testing.T) {
	// adrp x0, page 5 vs page 6; add x0,x0,#0x100 vs #0x200;
	// ldr x0,[x0,#0x10] vs #0x20 — all address-immediate deltas
	a := words(0x900000A0, 0x91004000, 0xF9400400)
	b := words(0x900040A0, 0x91008000, 0xF9400800)
	explained, unexplained, _ := parityWords(a, b)
	if unexplained != 0 || explained != 3 {
		t.Errorf("shifts: explained=%d unexplained=%d (want 3, 0)", explained, unexplained)
	}
}

func TestParityWordsRegisterChangeFails(t *testing.T) {
	// adrp x0 vs adrp x1 — different register is a real code difference
	a := words(0x90000000)
	b := words(0x90000001)
	_, unexplained, _ := parityWords(a, b)
	if unexplained != 1 {
		t.Errorf("register change: unexplained=%d (want 1)", unexplained)
	}
}

func TestParityWordsOpcodeChangeFails(t *testing.T) {
	// add vs sub with same immediates — genuinely different instruction
	a := words(0x91004000) // add x0, x0, #0x10
	b := words(0xD1004000) // sub x0, x0, #0x10
	_, unexplained, _ := parityWords(a, b)
	if unexplained != 1 {
		t.Errorf("opcode change: unexplained=%d (want 1)", unexplained)
	}
}

func TestParityWordsConstantChangeFails(t *testing.T) {
	// movz with different constant — not an address-forming pattern
	a := words(0xD2800000) // movz x0, #0
	b := words(0xD2800020) // movz x0, #1
	_, unexplained, _ := parityWords(a, b)
	if unexplained != 1 {
		t.Errorf("constant change: unexplained=%d (want 1)", unexplained)
	}
}

func TestParityWordsMixed(t *testing.T) {
	// one legit shift + one real difference
	a := words(0x900000A0, 0xAA0003E0)
	b := words(0x900040A0, 0xAA0003E1)
	explained, unexplained, _ := parityWords(a, b)
	if explained != 1 || unexplained != 1 {
		t.Errorf("mixed: explained=%d unexplained=%d (want 1, 1)", explained, unexplained)
	}
}
