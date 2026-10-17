//go:build amd64 && !purego

#include "textflag.h"

// func maskAVX2(b *byte, n int, key uint64)
// XORs n bytes at b, n a multiple of 32, with key repeated.
TEXT ·maskAVX2(SB), NOSPLIT, $0-24
	MOVQ         b+0(FP), DI
	MOVQ         n+8(FP), CX
	MOVQ         key+16(FP), AX
	MOVQ         AX, X0
	VPBROADCASTQ X0, Y0

loop128:
	CMPQ    CX, $128
	JB      loop32
	VPXOR   (DI), Y0, Y1
	VPXOR   32(DI), Y0, Y2
	VPXOR   64(DI), Y0, Y3
	VPXOR   96(DI), Y0, Y4
	VMOVDQU Y1, (DI)
	VMOVDQU Y2, 32(DI)
	VMOVDQU Y3, 64(DI)
	VMOVDQU Y4, 96(DI)
	ADDQ    $128, DI
	SUBQ    $128, CX
	JMP     loop128

loop32:
	CMPQ    CX, $32
	JB      done
	VPXOR   (DI), Y0, Y1
	VMOVDQU Y1, (DI)
	ADDQ    $32, DI
	SUBQ    $32, CX
	JMP     loop32

done:
	VZEROUPPER
	RET

// func maskASCIIAVX2(b *byte, n int, key uint64) uint32
// XORs n bytes at b, n a multiple of 32, with key repeated, and returns the
// high bits of the OR of every result byte position: zero when all of the
// unmasked bytes are ASCII.
TEXT ·maskASCIIAVX2(SB), NOSPLIT, $0-28
	MOVQ         b+0(FP), DI
	MOVQ         n+8(FP), CX
	MOVQ         key+16(FP), AX
	MOVQ         AX, X0
	VPBROADCASTQ X0, Y0
	VPXOR        Y5, Y5, Y5

asciiloop128:
	CMPQ    CX, $128
	JB      asciiloop32
	VPXOR   (DI), Y0, Y1
	VPXOR   32(DI), Y0, Y2
	VPXOR   64(DI), Y0, Y3
	VPXOR   96(DI), Y0, Y4
	VMOVDQU Y1, (DI)
	VMOVDQU Y2, 32(DI)
	VMOVDQU Y3, 64(DI)
	VMOVDQU Y4, 96(DI)
	VPOR    Y1, Y2, Y1
	VPOR    Y3, Y4, Y3
	VPOR    Y1, Y3, Y1
	VPOR    Y1, Y5, Y5
	ADDQ    $128, DI
	SUBQ    $128, CX
	JMP     asciiloop128

asciiloop32:
	CMPQ    CX, $32
	JB      asciidone
	VPXOR   (DI), Y0, Y1
	VMOVDQU Y1, (DI)
	VPOR    Y1, Y5, Y5
	ADDQ    $32, DI
	SUBQ    $32, CX
	JMP     asciiloop32

asciidone:
	VPMOVMSKB Y5, AX
	MOVL      AX, ret+24(FP)
	VZEROUPPER
	RET
