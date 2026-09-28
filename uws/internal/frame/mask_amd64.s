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
